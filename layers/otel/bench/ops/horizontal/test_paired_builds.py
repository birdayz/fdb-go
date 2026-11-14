"""Offline provenance/deployment regressions; no Bazel builds or benchmarks."""
import copy
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest import mock

import paired_builds as b
import paired_build_receipt as creator


class BuildFixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name).resolve()
        manifest = self.root / "source.manifest"
        manifest.write_text("synthetic source manifest\n")
        source = {"root": str(self.root), "revision": "a" * 40,
                  "manifest": str(manifest), "manifest_sha256": b._sha(manifest)}
        self.plan = {"fake_provider": True, "assets": {},
                     "sources": {arm: dict(source) for arm in ("go_pure", "go_native", "rust_native")}}
        library = self.root / "libfdb_c"
        library.write_bytes(b"synthetic native library\n")
        self.plan["assets"]["libfdb_c"] = self.seal(library)
        for name in b.EXECUTABLES:
            binary = self.root / name
            binary.write_bytes(("synthetic executable " + name + "\n").encode())
            self.plan["assets"].update(b.fake_artifacts(name, self.seal(binary), source, self.seal(library)))

    def seal(self, path):
        return {"path": str(path), "sha256": b._sha(path)}

    def receipt(self, name="control_native_binary"):
        return json.loads(Path(self.plan["assets"][name]["build_receipt"]["path"]).read_text())

    def save(self, value, name="control_native_binary"):
        path = Path(self.plan["assets"][name]["build_receipt"]["path"])
        path.write_text(json.dumps(value, sort_keys=True) + "\n")
        self.plan["assets"][name]["build_receipt"] = self.seal(path)

    def validate(self, name="control_native_binary", **kwargs):
        return b.validate(self.plan, name, inspect_elf=False, **kwargs)

    def request(self, name="control_native_binary", destination=None):
        ref = self.plan["assets"][b.deployment_asset(name)]
        return {"archive": ref["path"], "sha256": ref["sha256"],
                "destination": str(destination or self.root / (name + ".bundle")), "files": self.receipt(name)["files"]}

    def tar(self, members):
        """members: [(path, bytes, type, mode)], retaining malicious headers."""
        path = self.root / "custom.tar"
        with tarfile.open(path, "w", format=tarfile.USTAR_FORMAT) as bundle:
            for name, data, kind, mode in members:
                member = tarfile.TarInfo(name)
                member.type, member.mode = kind, mode
                member.size = len(data) if kind == tarfile.REGTYPE else 0
                if kind in (tarfile.SYMTYPE, tarfile.LNKTYPE):
                    member.linkname = "../../escape"
                bundle.addfile(member, io.BytesIO(data) if member.isreg() else None)
        return path

    def one_file(self, path="bin/exe", data=b"executable", mode=0o755):
        return {"path": path, "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data), "mode": mode}

    def install_tar(self, archive, files):
        return b.install_bundle({"archive": str(archive), "sha256": b._sha(archive),
                                 "destination": str(self.root / "custom.bundle"), "files": files})


class ReceiptToolTests(BuildFixture):
    def args(self, name, output):
        receipt = self.receipt(name)
        command = self.root / (name + "-command.json")
        command.write_text(json.dumps(receipt["build_command"]))
        args = ["--executable-asset", name, "--binary", self.plan["assets"][name]["path"],
                "--source-root", str(self.root), "--source-revision", "a" * 40,
                "--source-manifest", str(self.root / "source.manifest"), "--developer-home", str(self.root / "home"),
                "--build-command-file", str(command), "--build-exit-code", "0",
                "--deployment", self.plan["assets"][b.deployment_asset(name)]["path"],
                "--executable-relative-path", receipt["executable_relative_path"], "--output", str(output)]
        if name == "control_native_binary":
            args.extend(["--client-library-relative-path", receipt["client_library_relative_path"],
                         "--client-library", self.plan["assets"]["libfdb_c"]["path"]])
        return args

    def test_receipt_creator_validates_all_roles_and_publishes_exclusively_private(self):
        for name in b.EXECUTABLES:
            output = self.root / (name + "-created.json")
            args = self.args(name, output)
            with mock.patch.object(b, "readelf_dynamic", return_value=b.fake_readelf(name)), mock.patch("sys.stdout", new=io.StringIO()):
                entries = creator.main(args)
                self.assertEqual(entries[name]["build_receipt"], self.seal(output))
                self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
                plan = copy.deepcopy(self.plan); plan["fake_provider"] = False; plan["assets"].update(entries)
                self.assertEqual(b.validate(plan, name), json.loads(output.read_text()))
                before = output.read_bytes()
                with self.assertRaisesRegex(ValueError, "already exists"):
                    creator.main(args)
                self.assertEqual(output.read_bytes(), before)

    def test_creator_does_not_publish_invalid_nohome_provenance(self):
        name = "control_pure_binary"; output = self.root / "invalid-receipt.json"
        args = self.args(name, output)
        command = self.root / (name + "-command.json")
        command.write_text(json.dumps(["bazelisk", "build", "//synthetic:pure"]))
        with mock.patch.object(b, "readelf_dynamic", return_value=b.fake_readelf(name)), self.assertRaisesRegex(ValueError, "nohome_rc"):
            creator.main(args)
        self.assertFalse(output.exists())

    def test_schema_and_all_three_literal_examples_match_closed_receipt_fields(self):
        root = Path(__file__).parent
        schema = json.loads((root / "paired-build-receipt-v1.schema.json").read_text())
        self.assertEqual(set(schema["required"]), set(b.RECEIPT_FIELDS))
        self.assertFalse(schema["additionalProperties"])
        self.assertEqual(set(schema["$defs"]["deployment_file"]["required"]), {"path", "sha256", "bytes", "mode"})
        for name in b.EXECUTABLES:
            example = json.loads((root / "build-receipt-examples" / (name + ".json")).read_text())
            self.assertEqual(set(example), set(b.RECEIPT_FIELDS))
            self.assertEqual(example["executable_asset"], name)
            self.assertEqual(example["deployment_asset"], b.deployment_asset(name))
            self.assertEqual(example["files"], sorted(example["files"], key=lambda row: row["path"]))
            self.assertIn("--nohome_rc", example["build_command"])


class ProvenanceTests(BuildFixture):
    def test_positive_receipts_and_live_readelf_for_all_roles(self):
        for name in b.EXECUTABLES:
            with self.subTest(name=name), mock.patch.object(b, "readelf_dynamic", return_value=b.fake_readelf(name)) as readelf:
                value = b.validate(self.plan, name)
                self.assertEqual(value, self.receipt(name))
                readelf.assert_called_once_with(Path(self.plan["assets"][name]["path"]))
                self.assertEqual(b.read_receipt(self.plan, name), value)

    def test_paid_validation_cannot_disable_live_inspection(self):
        self.plan["fake_provider"] = False
        with self.assertRaisesRegex(ValueError, "cannot skip live ELF"):
            self.validate()
        with mock.patch.object(b, "readelf_dynamic", return_value=b.fake_readelf("control_native_binary")):
            b.validate(self.plan, "control_native_binary")

    def test_lightweight_receipt_does_not_read_bundle_or_binary(self):
        name = "control_native_binary"
        Path(self.plan["assets"][name]["path"]).unlink()
        Path(self.plan["assets"][b.deployment_asset(name)]["path"]).unlink()
        self.assertEqual(b.read_receipt(self.plan, name)["executable_asset"], name)

    def test_omitted_build_receipt_and_unknown_receipt_fields(self):
        asset = self.plan["assets"]["control_native_binary"]
        ref = asset.pop("build_receipt")
        with self.assertRaisesRegex(ValueError, "build_receipt"):
            self.validate()
        asset["build_receipt"] = ref
        value = self.receipt(); value["unexpected"] = True; self.save(value)
        with self.assertRaisesRegex(ValueError, "exact build receipt"):
            self.validate()

    def test_receipt_rejects_duplicate_keys_and_nonfinite_json(self):
        name = "control_native_binary"
        path = Path(self.plan["assets"][name]["build_receipt"]["path"])
        original = path.read_text()
        invalid = [original.replace('"build_exit_code": 0', '"build_exit_code": 0, "build_exit_code": 0')]
        invalid += [original.replace('"build_exit_code": 0', '"build_exit_code": ' + constant)
                    for constant in ("NaN", "Infinity", "-Infinity", "1e999")]
        for text in invalid:
            path.write_text(text)
            self.plan["assets"][name]["build_receipt"] = self.seal(path)
            with self.subTest(text=text), self.assertRaisesRegex(ValueError, "invalid build receipt"):
                self.validate()
            with self.assertRaisesRegex(ValueError, "invalid build receipt"):
                b.read_receipt(self.plan, name)

    def test_nohome_is_startup_option_not_build_option(self):
        original = self.receipt()
        for command in (["bazelisk", "build", "//x"], ["bazel", "build", "--nohome_rc", "//x"],
                        ["bazel", "--nohome_rc", "--home_rc", "build", "//x"],
                        ["bazel", "--nohome_rc", "--home_rc=false", "build", "//x"],
                        ["bazel", "--nohome_rc", "--nohome_rc=false", "build", "//x"],
                        ["sh", "--nohome_rc", "build", "//x"]):
            value = copy.deepcopy(original); value["build_command"] = command; self.save(value)
            with self.subTest(command=command), self.assertRaises(ValueError):
                self.validate()

    def test_failed_build_or_readelf_and_wrong_readelf_command(self):
        original = self.receipt()
        mutations = (("build_exit_code", 1), ("build_exit_code", False), ("readelf_exit_code", 1),
                     ("readelf_exit_code", False), ("readelf_command", ["readelf", "-d", "/wrong/binary"]),
                     ("readelf_output", ""))
        for key, value in mutations:
            receipt = copy.deepcopy(original); receipt[key] = value; self.save(receipt)
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                self.validate()

    def test_source_identity_and_executable_deployment_binding(self):
        original = self.receipt()
        for key, value in (("binary_sha256", "b" * 64), ("source_revision", "b" * 40),
                           ("source_manifest_sha256", "b" * 64), ("deployment_sha256", "b" * 64),
                           ("executable_asset", "rust_binary"), ("deployment_asset", "rust_binary_deployment")):
            receipt = copy.deepcopy(original); receipt[key] = value; self.save(receipt)
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.validate()

    def test_home_rpath_and_runpath_forbidden_but_origin_allowed(self):
        original = self.receipt()
        for kind in ("RPATH", "RUNPATH"):
            value = copy.deepcopy(original)
            value["readelf_output"] += " 0x000000000000001d (%s) Library runpath: [/home/developer/lib]\n" % kind
            self.save(value)
            with self.subTest(kind=kind), self.assertRaisesRegex(ValueError, "RPATH/RUNPATH"):
                self.validate()
        original["readelf_output"] += " 0x000000000000001d (RUNPATH) Library runpath: [$ORIGIN/control_native_binary.runfiles/lib]\n"
        self.save(original)
        self.validate()

    def test_native_requires_needed_and_pure_rust_forbid_prelink(self):
        for name in b.EXECUTABLES:
            value = self.receipt(name)
            value["readelf_output"] = (b.fake_readelf("rust_binary") if name == "control_native_binary"
                                       else b.fake_readelf("control_native_binary"))
            self.save(value, name)
            with self.subTest(name=name), self.assertRaisesRegex(ValueError, "DT_NEEDED"):
                self.validate(name)

    def test_live_readelf_must_match_capture_and_minimal_environment(self):
        with mock.patch.object(b, "readelf_dynamic", return_value="different\n"):
            with self.assertRaisesRegex(ValueError, "live readelf"):
                b.validate(self.plan, "control_native_binary")
        with mock.patch.object(b.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "captured\n", "")) as run:
            binary = self.plan["assets"]["control_native_binary"]["path"]
            self.assertEqual(b.readelf_dynamic(binary), "captured\n")
            self.assertEqual(run.call_args.args[0], ["readelf", "-d", binary])
            self.assertEqual(run.call_args.kwargs["env"], {"PATH": "/usr/bin:/bin", "LC_ALL": "C"})
        with mock.patch.object(b.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "not ELF")):
            with self.assertRaisesRegex(ValueError, "readelf -d failed"):
                b.readelf_dynamic(binary)

    def test_changed_binary_bundle_receipt_and_source_manifest(self):
        name = "control_native_binary"
        paths = [Path(self.plan["assets"][name]["path"]),
                 Path(self.plan["assets"][b.deployment_asset(name)]["path"]),
                 Path(self.plan["assets"][name]["build_receipt"]["path"]),
                 Path(self.plan["sources"]["go_native"]["manifest"])]
        for path in paths:
            original = path.read_bytes(); path.write_bytes(original + b"changed\n")
            with self.subTest(path=path.name), self.assertRaises(ValueError):
                self.validate()
            path.write_bytes(original)

    def test_check_assets_false_skips_live_artifact_checks(self):
        Path(self.plan["assets"]["control_native_binary"]["path"]).write_bytes(b"changed")
        with mock.patch.object(b, "readelf_dynamic", side_effect=AssertionError("unexpected live ELF scan")):
            self.validate(check_assets=False)

    def test_repository_rc_required_hashed_and_changed_rc_rejected(self):
        rc = self.root / ".bazelrc"; rc.write_text("build --color=no\n")
        value = self.receipt()
        with self.assertRaisesRegex(ValueError, "repository .bazelrc"):
            self.validate()
        value["rc_set"] = [self.seal(rc)]; self.save(value)
        self.validate()
        rc.write_text("build --color=yes\n")
        with self.assertRaisesRegex(ValueError, "hash mismatch"):
            self.validate()

    def test_home_rc_explicit_path_symlink_and_hardlink_identity_forbidden(self):
        home = self.root / "home"; home.mkdir()
        home_rc = home / ".bazelrc"; home_rc.write_text("build --jobs=1\n")
        original = self.receipt(); original["developer_home"] = str(home)
        for separate in (False, True):
            value = copy.deepcopy(original)
            option = ["--bazelrc", str(home_rc)] if separate else ["--bazelrc=" + str(home_rc)]
            value["build_command"][2:2] = option; self.save(value)
            with self.subTest(separate=separate), self.assertRaisesRegex(ValueError, "home .bazelrc"):
                self.validate()
        link = self.root / "home-rc-link"; link.symlink_to(home_rc)
        hardlink = self.root / "home-rc-hardlink"; os.link(home_rc, hardlink)
        for path in (home_rc, link, hardlink):
            value = copy.deepcopy(original); value["rc_set"] = [self.seal(path)]; self.save(value)
            with self.subTest(path=path.name), self.assertRaises(ValueError):
                self.validate()

    def test_explicit_repo_rc_must_be_in_effective_set(self):
        rc = self.root / "explicit.rc"; rc.write_text("build --color=no\n")
        value = self.receipt(); value["build_command"][2:2] = ["--bazelrc=" + str(rc)]; self.save(value)
        with self.assertRaisesRegex(ValueError, "effective rc_set"):
            self.validate()
        value["rc_set"] = [self.seal(rc)]; self.save(value)
        self.validate()

    def test_native_canonical_library_name_parent_and_hash(self):
        original = self.receipt()
        for relative in ("libfdb_c.so.7", "a:b/libfdb_c.so", "../libfdb_c.so", "/libfdb_c.so", None):
            value = copy.deepcopy(original); value["client_library_relative_path"] = relative; self.save(value)
            with self.subTest(relative=relative), self.assertRaises(ValueError):
                self.validate()
        self.save(original)
        self.plan["assets"]["libfdb_c"]["sha256"] = "b" * 64
        with self.assertRaisesRegex(ValueError, "native runfile"):
            self.validate()


class BundleTests(BuildFixture):
    def test_positive_manifest_install_bytes_modes_and_existing_destination(self):
        for name in b.EXECUTABLES:
            request = self.request(name)
            manifest = b.archive_manifest(request["archive"])
            self.assertEqual(manifest, request["files"])
            receipt = b.install_bundle(request)
            self.assertEqual(receipt["schema"], "otel_paired_deployment_receipt_v1")
            self.assertEqual(receipt["files"], manifest)
            for item in manifest:
                path = Path(receipt["destination"]) / item["path"]
                self.assertEqual(b._sha(path), item["sha256"])
                self.assertEqual(stat.S_IMODE(path.stat().st_mode), item["mode"])
            with self.assertRaisesRegex(ValueError, "already exists"):
                b.install_bundle(request)

    def test_self_contained_installer_executes_without_checkout(self):
        request = self.request("rust_binary")
        request_path = self.root / "install-request.json"
        request_path.write_text(json.dumps(request))
        result = subprocess.run([sys.executable, "-I", "-c", b.INSTALL_SCRIPT, str(request_path), b._sha(request_path)],
                                cwd=self.root, env={"PATH": "/usr/bin:/bin"}, capture_output=True, text=True, check=True)
        self.assertEqual(json.loads(result.stdout)["files"], request["files"])
        request_path.write_text("{}")
        failed = subprocess.run([sys.executable, "-I", "-c", b.INSTALL_SCRIPT, str(request_path), "a" * 64],
                                cwd=self.root, env={"PATH": "/usr/bin:/bin"}, capture_output=True, text=True, check=False)
        self.assertNotEqual(failed.returncode, 0)
        self.assertIn("sealed bounded install request", failed.stderr)

    def test_empty_runfiles_directory_is_preserved_and_admitted(self):
        name = "rust_binary"; value = self.receipt(name)
        binary = Path(self.plan["assets"][name]["path"]).read_bytes()
        relative = value["executable_relative_path"]
        archive = self.tar([(relative, binary, tarfile.REGTYPE, 0o755),
                            (relative + ".runfiles/", b"", tarfile.DIRTYPE, 0o755)])
        value["files"] = b.archive_manifest(archive)
        value["deployment_sha256"] = b._sha(archive)
        self.plan["assets"][b.deployment_asset(name)] = self.seal(archive); self.save(value, name)
        self.validate(name)
        receipt = b.install_bundle(self.request(name))
        self.assertTrue((Path(receipt["destination"]) / (relative + ".runfiles")).is_dir())

    def test_manifest_order_is_canonical_but_archive_order_need_not_be(self):
        value = self.receipt()
        value["files"] = list(reversed(value["files"]))
        self.save(value)
        with self.assertRaisesRegex(ValueError, "sorted by path"):
            self.validate()
        with self.assertRaisesRegex(ValueError, "sorted by path"):
            b.read_receipt(self.plan, "control_native_binary")
        with self.assertRaisesRegex(ValueError, "sorted by path"):
            b.install_bundle(self.request())
        archive = self.tar([("z", b"z", tarfile.REGTYPE, 0o644), ("a", b"a", tarfile.REGTYPE, 0o644)])
        self.assertEqual([item["path"] for item in b.archive_manifest(archive)], ["a", "z"])

    def test_compressed_archive_uses_same_bounded_validation(self):
        archive = self.tar([("bin/exe", b"executable", tarfile.REGTYPE, 0o755)])
        compressed = self.root / "compressed.tar.gz"
        compressed.write_bytes(gzip.compress(archive.read_bytes()))
        manifest = b.archive_manifest(compressed)
        self.assertEqual(manifest, [self.one_file()])
        self.install_tar(compressed, manifest)

    def test_missing_runfiles_tree_rejected(self):
        name = "rust_binary"; value = self.receipt(name)
        relative = value["executable_relative_path"]
        binary = Path(self.plan["assets"][name]["path"]).read_bytes()
        archive = self.tar([(relative, binary, tarfile.REGTYPE, 0o755)])
        value["files"] = b.archive_manifest(archive); value["deployment_sha256"] = b._sha(archive)
        self.plan["assets"][b.deployment_asset(name)] = self.seal(archive); self.save(value, name)
        with self.assertRaisesRegex(ValueError, "runfiles tree"):
            self.validate(name)

    def test_unsafe_paths_links_special_and_duplicate_entries_rejected(self):
        cases = [[(name, b"x", tarfile.REGTYPE, 0o755)] for name in
                 ("../escape", "/escape", "bin/../../escape", "bin//exe", "./exe", "bin/./exe", "bin\\exe")]
        cases += [[("link", b"", kind, 0o755)] for kind in
                  (tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.FIFOTYPE, tarfile.CHRTYPE, tarfile.BLKTYPE)]
        cases += [[("bin/exe", b"x", tarfile.REGTYPE, 0o755), ("bin/exe", b"x", tarfile.REGTYPE, 0o755)]]
        for members in cases:
            archive = self.tar(members)
            with self.subTest(members=members), self.assertRaises(ValueError):
                b.archive_manifest(archive)
            with self.assertRaises(ValueError):
                self.install_tar(archive, [self.one_file(data=b"x")])
            self.assertFalse((self.root / "custom.bundle").exists())
        self.assertFalse((self.root / "escape").exists())
        self.assertFalse(list(self.root.glob(".custom.bundle-*")))

    def test_file_directory_prefix_collision_rejected(self):
        archive = self.tar([("bin", b"x", tarfile.REGTYPE, 0o755),
                            ("bin/nested", b"y", tarfile.REGTYPE, 0o755)])
        with self.assertRaisesRegex(ValueError, "collision"):
            b.archive_manifest(archive)

    def test_bad_hash_contents_size_mode_and_manifest_entries(self):
        good = self.one_file()
        archive = self.tar([("bin/exe", b"executable", tarfile.REGTYPE, 0o755)])
        cases = []
        for key, value in (("sha256", "0" * 64), ("bytes", 1), ("mode", 0o644),
                           ("mode", 0o777), ("path", "../exe"), ("bytes", True)):
            item = dict(good); item[key] = value; cases.append([item])
        cases += [[good, good], [], [good, self.one_file("missing")]]
        for files in cases:
            with self.subTest(files=files), self.assertRaises(ValueError):
                self.install_tar(archive, files)
        request = self.request(); request["sha256"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "archive hash mismatch"):
            b.install_bundle(request)
        self.assertFalse(Path(request["destination"]).exists())

    def test_oversize_and_file_count_bounds(self):
        value = self.one_file(); value["bytes"] = b.MAX_BYTES + 1
        with self.assertRaisesRegex(ValueError, "bounded file"):
            b._manifest([value])
        with mock.patch.object(b, "MAX_FILES", 1):
            with self.assertRaisesRegex(ValueError, "bounded deployment"):
                b._manifest([self.one_file(), self.one_file("second")])
        archive = self.tar([("bin/exe", b"executable", tarfile.REGTYPE, 0o755)])
        with mock.patch.object(b, "MAX_BYTES", 100):
            with self.assertRaisesRegex(ValueError, "bounded regular"):
                b.archive_manifest(archive)

    def test_unsafe_install_destination_and_symlink_archive(self):
        existing = self.root / "existing"; existing.mkdir()
        link = self.root / "linked-parent"; link.symlink_to(existing, target_is_directory=True)
        for destination in (str(existing), str(link / "bundle"), str(self.root / ".." / "escape"), "relative"):
            request = self.request(destination=Path(destination))
            with self.subTest(destination=destination), self.assertRaises(ValueError):
                b.install_bundle(request)
        archive = Path(self.request()["archive"])
        linked_archive = self.root / "linked.tar"; linked_archive.symlink_to(archive)
        with self.assertRaisesRegex(ValueError, "bounded regular"):
            self.install_tar(linked_archive, self.receipt()["files"])

    def test_truncated_tar_rejected_and_partial_install_cleaned(self):
        archive = self.tar([("bin/exe", b"x" * 4096, tarfile.REGTYPE, 0o755)])
        archive.write_bytes(archive.read_bytes()[:600])
        with self.assertRaises(ValueError):
            self.install_tar(archive, [self.one_file(data=b"x" * 4096)])
        self.assertFalse((self.root / "custom.bundle").exists())
        self.assertFalse(list(self.root.glob(".custom.bundle-*")))


if __name__ == "__main__":
    unittest.main()
