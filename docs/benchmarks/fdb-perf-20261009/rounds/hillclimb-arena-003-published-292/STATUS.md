#003 preparation on published292b79226 — no campaign or integration

Root request447 authorizes scratch preparation on published292b79226 only, not the read-order WIP. Exact base commit:292b79226f5ca904add77da24bb4738b89651eec (`fix(fdbgo): preserve ownership and streamline local reads`). Created using git archive over pkg/fdbgo,go.mod,go.sum,MODULE.bazel,MODULE.bazel.lock;749 regular files. No active or ready worktree file was copied or edited.

Base tar SHA2566f045f9bd6d7840191c33f65e22c7c4abf9d5352d286965ae336c110d76d0b56. Base manifest SHA256716052ddde9ff9cc6f32810b72f562c116e647f87dd7c1bafd4b2fce6160136b. Candidate manifest SHA25665769101516d47047bbe053175bbfea585c07822c3f7aa82e2ea2d08be2a3b8f.

Applied the API-compatible003 patch3f2158699f0166049bb649b90a16a821c212e7bf1fd087757c23858cb441d831 using git apply --check then git apply. Regenerated diff against this exact published parent: arena-003-from-published-292.patch SHA256bc57f249650603cf657a2f86c8e05351de33b9c51a8b33708e45c5fadd0fe9ce. Only three files differ:transaction.go,conflictbuf_unit_test.go,transaction_unit_test.go. Both749-file manifests verified.

The production delta uses fixed512B/32KiB append-only slabs, retained linked pool owners, direct exact-size unpooled requests above32KiB, and reset retirement only after turnover drains users and vectors/RYW clear. No prefix copy. Return-loop next is saved and cleared before Put. Retry keeps one bounded current slab; postcommit returns all. Tests use published Get/Snapshot.Get+execution-lease APIs and are self-contained in the dedicated fast test target.

No replay002 overlay exists in this candidate. This is preparation, not the required future benchmark baseline. Per root447, wait for root's **published baseline including replay002** and rebase before any campaign. Every future campaign/control must include CGo and publish absolute mean/p99/CPU plus paired Go/CGo and old-Go attribution tables. Preserve previous WIP results under their old identities; do not relabel them as PR measurements.

No compilation, Bazel execution, remote build, profile, timing, provisioning or deletion performed. Root owns integration and normal Bazel verification.003 is NOT integrated in PR789. Existing C++/codehealth ACKs concern the earlier scratch patches and are historical review evidence, not the final PR review: root now requires final review per PR with Opus5.5xhigh.
