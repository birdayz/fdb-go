//! Isolated dynamic binding to the repository-pinned libfdb_c 7.3.77, API 730.
//! Signatures/layouts: FoundationDB 7.3.77 bindings/c/foundationdb/fdb_c.h.
use crate::{
    Error, ErrorKind, Result,
    writer::{Context, Database, KeyValues, Transaction},
};
use libloading::Library;
use sha2::{Digest, Sha256};
use std::{
    collections::HashMap,
    ffi::{CString, OsString, c_char, c_void},
    fs,
    io::Read,
    os::unix::fs::MetadataExt,
    path::Path,
    ptr,
    sync::{
        Arc, Mutex, OnceLock,
        atomic::{AtomicBool, AtomicI32, AtomicUsize, Ordering},
        mpsc,
    },
    thread,
    time::Duration,
};

type Handle = *mut c_void;
type Code = i32;
type Callback = unsafe extern "C" fn(Handle, *mut c_void);
type Select = unsafe extern "C" fn(i32, i32) -> Code;
type NetworkOption = unsafe extern "C" fn(i32, *const u8, i32) -> Code;
type Setup = unsafe extern "C" fn() -> Code;
/// fdb.options `disable_multi_version_client_api`; it must precede network setup.
const DISABLE_MULTI_VERSION_CLIENT_API: i32 = 60;
const ENV_OPTION_PREFIX: &[u8] = b"FDB_NETWORK_OPTION_";
const ENV_OPTION_REFUSED: &str =
    "inherited FDB_NETWORK_OPTION_* environment variables are not allowed";
/// SHA256 of the only loadable libfdb_c, pinned in MODULE.bazel.
pub const CLIENT_SHA256: &str = "617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa";
static NETWORK_STARTED: AtomicBool = AtomicBool::new(false);
const NETWORK_RUNNING: i32 = i32::MIN;
// libfdb_c is a process-global, one-shot runtime. Pin executable code until
// process exit, including callbacks that outlive a fatal network-loop failure.
static LOADED_API: OnceLock<Arc<Api>> = OnceLock::new();

#[repr(C, packed(4))]
#[derive(Clone, Copy)]
struct KeyValue {
    key: *const u8,
    key_length: i32,
    value: *const u8,
    value_length: i32,
}

struct Api {
    _library: Library,
    select: Select,
    network_option: NetworkOption,
    setup: Setup,
    run: unsafe extern "C" fn() -> Code,
    stop: unsafe extern "C" fn() -> Code,
    open: unsafe extern "C" fn(*const c_char, *mut Handle) -> Code,
    close: unsafe extern "C" fn(Handle),
    create: unsafe extern "C" fn(Handle, *mut Handle) -> Code,
    destroy: unsafe extern "C" fn(Handle),
    cancel: unsafe extern "C" fn(Handle),
    option: unsafe extern "C" fn(Handle, i32, *const u8, i32) -> Code,
    get: unsafe extern "C" fn(Handle, *const u8, i32, i32) -> Handle,
    range: unsafe extern "C" fn(
        Handle,
        *const u8,
        i32,
        i32,
        i32,
        *const u8,
        i32,
        i32,
        i32,
        i32,
        i32,
        i32,
        i32,
        i32,
        i32,
    ) -> Handle,
    set: unsafe extern "C" fn(Handle, *const u8, i32, *const u8, i32),
    clear: unsafe extern "C" fn(Handle, *const u8, i32),
    size: unsafe extern "C" fn(Handle) -> Handle,
    commit: unsafe extern "C" fn(Handle) -> Handle,
    on_error: unsafe extern "C" fn(Handle, Code) -> Handle,
    future_destroy: unsafe extern "C" fn(Handle),
    future_callback: unsafe extern "C" fn(Handle, Callback, *mut c_void) -> Code,
    future_error: unsafe extern "C" fn(Handle) -> Code,
    future_value: unsafe extern "C" fn(Handle, *mut i32, *mut *const u8, *mut i32) -> Code,
    future_kvs: unsafe extern "C" fn(Handle, *mut *const KeyValue, *mut i32, *mut i32) -> Code,
    future_int64: unsafe extern "C" fn(Handle, *mut i64) -> Code,
}

/// libfdb_c applies FDB_NETWORK_OPTION_* variables at setup even with option 60, so any
/// such key (raw bytes on Unix) is refused without reporting its name or value.
fn refuse_network_option_env(vars: impl IntoIterator<Item = (OsString, OsString)>) -> Result<()> {
    if vars
        .into_iter()
        .any(|(key, _)| key.as_encoded_bytes().starts_with(ENV_OPTION_PREFIX))
    {
        return Err(Error::new(ErrorKind::Config, ENV_OPTION_REFUSED));
    }
    Ok(())
}

/// Selects API 730 and disables the multi-version client API before network setup.
/// Safety: the pointers are the loaded libfdb_c's symbols, called once per process.
unsafe fn start_network(select: Select, option: NetworkOption, setup: Setup) -> Result<()> {
    unsafe {
        checked(select(730, 730))?;
        checked(option(DISABLE_MULTI_VERSION_CLIENT_API, ptr::null(), 0))?;
        checked(setup())
    }
}

/// The rule that proved every mapping of the loaded libfdb_c is the sealed artifact.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum MappingRule {
    /// Each mapping names the artifact's own stat device.
    ExactStat,
    /// mountinfo proves the artifact's mount is btrfs, whose mappings name the
    /// superblock device rather than the subvolume's stat device.
    BtrfsSuperblock,
}

/// Proof, read from this process's own mappings right after the client was loaded, that
/// the loaded libfdb_c is the sealed artifact. Only a successful native open creates one.
#[derive(Clone, Copy, Debug)]
pub struct MappedClientProof {
    rule: MappingRule,
}
impl MappedClientProof {
    /// `exact_stat` or `btrfs_superblock`.
    pub fn rule(&self) -> &'static str {
        match self.rule {
            MappingRule::ExactStat => "exact_stat",
            MappingRule::BtrfsSuperblock => "btrfs_superblock",
        }
    }
}

const NOT_THE_ARTIFACT: &str = "a mapped native client is not the sealed job artifact";

fn mapping_error(message: &'static str) -> Error {
    Error::new(ErrorKind::Config, message)
}

/// A Linux device number as major:minor.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Device {
    major: u64,
    minor: u64,
}

/// Splits a userspace st_dev the way glibc's gnu_dev_major and gnu_dev_minor do.
fn stat_device(dev: u64) -> Device {
    Device {
        major: ((dev & 0x0000_0000_000f_ff00) >> 8) | ((dev & 0xffff_f000_0000_0000) >> 32),
        minor: (dev & 0x0000_0000_0000_00ff) | ((dev & 0x0000_0fff_fff0_0000) >> 12),
    }
}

/// A file as the kernel identifies it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct FileIdentity {
    dev: u64,
    ino: u64,
}
impl FileIdentity {
    fn of(meta: &fs::Metadata) -> Self {
        Self {
            dev: meta.dev(),
            ino: meta.ino(),
        }
    }
}

/// A nonempty all-digit decimal (or hexadecimal) number: no sign, prefix or space.
fn number(s: &str, radix: u32) -> Option<u64> {
    if s.is_empty() || !s.chars().all(|c| c.is_digit(radix)) {
        return None;
    }
    u64::from_str_radix(s, radix).ok()
}

/// A maps line's hexadecimal major:minor device (32-bit halves) and nonzero decimal inode.
fn mapped_identity(dev: &str, inode: &str) -> Option<(Device, u64)> {
    let (major, minor) = dev.split_once(':')?;
    let half = |s| number(s, 16).filter(|&n| n <= u64::from(u32::MAX));
    let ino = number(inode, 10).filter(|&n| n != 0)?;
    Some((
        Device {
            major: half(major)?,
            minor: half(minor)?,
        },
        ino,
    ))
}

/// An absolute path in its clean form with no escape (`\040` for a space, ...); nothing
/// is decoded or cleaned into a match.
fn clean_path(p: &str) -> bool {
    p == "/"
        || (p.starts_with('/')
            && !p.contains('\\')
            && p[1..]
                .split('/')
                .all(|c| !c.is_empty() && c != "." && c != ".."))
}

/// One strictly parsed mountinfo line (proc(5)).
#[derive(Debug)]
struct Mount {
    device: Device,
    root: String,
    point: String,
    fstype: String,
}

/// Parses every line or none: decimal IDs and major:minor, a root, a clean mount point,
/// options, optional fields, the `-` separator, then exactly the filesystem type, source
/// and super options. A root need not be a path (nsfs shows `net:[inode]`); it is
/// checked wherever it matters, by `longest_containing`.
fn parse_mountinfo(text: &str) -> Option<Vec<Mount>> {
    let mut out = Vec::new();
    for line in text.strip_suffix('\n').unwrap_or(text).split('\n') {
        let fields: Vec<&str> = line.split(' ').collect();
        let sep = (6..fields.len()).find(|&i| fields[i] == "-")?;
        if fields.len() != sep + 4 || fields.iter().any(|f| f.is_empty()) {
            return None;
        }
        number(fields[0], 10)?;
        number(fields[1], 10)?;
        let (major, minor) = fields[2].split_once(':')?;
        let half = |s| number(s, 10).filter(|&n| n <= u64::from(u32::MAX));
        if !clean_path(fields[4]) {
            return None;
        }
        out.push(Mount {
            device: Device {
                major: half(major)?,
                minor: half(minor)?,
            },
            root: fields[3].into(),
            point: fields[4].into(),
            fstype: fields[sep + 1].into(),
        });
    }
    Some(out)
}

/// The mounts whose point contains `path` at a component boundary and is longest, or
/// None if any containing mount's root is not a clean path.
fn longest_containing<'a>(mounts: &'a [Mount], path: &str) -> Option<Vec<&'a Mount>> {
    let mut best: Vec<&Mount> = Vec::new();
    for m in mounts {
        let within = m.point == "/"
            || path == m.point
            || path
                .strip_prefix(m.point.as_str())
                .is_some_and(|rest| rest.starts_with('/'));
        if !within {
            continue;
        }
        if !clean_path(&m.root) {
            return None;
        }
        match best.first().map(|b| b.point.len()) {
            Some(len) if len > m.point.len() => {}
            Some(len) if len == m.point.len() => best.push(m),
            _ => best = vec![m],
        }
    }
    Some(best)
}

/// The superblock device of `path`'s mount when mountinfo proves it unambiguously: the
/// unique longest containing mount, of type btrfs. Anything else gives no proof.
fn btrfs_device(mountinfo: &str, path: &str) -> Option<Device> {
    let mounts = parse_mountinfo(mountinfo)?;
    match longest_containing(&mounts, path)?.as_slice() {
        [m] if m.fstype == "btrfs" => Some(m.device),
        _ => None,
    }
}

/// Requires every mapped libfdb_c, whatever its version suffix, to be `artifact`: the
/// maps inode is the artifact's, the mapped pathname still names it, and the maps device
/// is the artifact's stat device or, only when mountinfo proves the pathname's mount is
/// btrfs, that mount's superblock device. Returns the one rule that proved them all and
/// how many mappings it checked. A deleted, ambiguous, malformed, replaced, second or
/// absent client, or mixed rules, fail with a message naming no path.
fn check_mapped_client(
    maps: &str,
    mountinfo: &str,
    artifact: FileIdentity,
    stat: impl Fn(&str) -> Option<FileIdentity>,
) -> Result<(MappingRule, usize)> {
    let exact = stat_device(artifact.dev);
    let mut rule = None;
    let mut checked = 0;
    for line in maps.split('\n') {
        let fields: Vec<&str> = line.split_whitespace().collect();
        if fields.len() < 6 {
            continue;
        }
        let pathname = fields[5..].join(" ");
        if !pathname
            .rsplit('/')
            .next()
            .unwrap_or("")
            .contains("libfdb_c")
        {
            continue;
        }
        checked += 1;
        if fields.len() != 6 {
            return Err(mapping_error(
                "a mapped native client is deleted or ambiguous",
            ));
        }
        let (dev, ino) = mapped_identity(fields[3], fields[4])
            .ok_or_else(|| mapping_error("a native client mapping is malformed"))?;
        let named = stat(fields[5]);
        if ino != artifact.ino || named != Some(artifact) {
            return Err(mapping_error(NOT_THE_ARTIFACT));
        }
        let proved = if dev == exact {
            MappingRule::ExactStat
        } else if btrfs_device(mountinfo, fields[5]) == Some(dev) {
            MappingRule::BtrfsSuperblock
        } else {
            return Err(mapping_error(NOT_THE_ARTIFACT));
        };
        if rule.is_some_and(|r| r != proved) {
            return Err(mapping_error(
                "the native client's mappings were proved by different rules",
            ));
        }
        rule = Some(proved);
    }
    rule.map(|r| (r, checked))
        .ok_or_else(|| mapping_error("the pinned native client is not mapped"))
}

/// Reads this process's mappings and mounts with `read` (production: the real files),
/// failing closed on any read error, invalid UTF-8 included, and proves the loaded
/// client is `artifact`.
fn prove_mapped(
    read: impl Fn(&str) -> std::io::Result<String>,
    artifact: FileIdentity,
) -> Result<MappingRule> {
    let maps =
        read("/proc/self/maps").map_err(|_| mapping_error("cannot read the process mappings"))?;
    let mountinfo = read("/proc/self/mountinfo")
        .map_err(|_| mapping_error("cannot read the process mounts"))?;
    check_mapped_client(&maps, &mountinfo, artifact, |p| {
        fs::metadata(p).ok().map(|m| FileIdentity::of(&m))
    })
    .map(|(rule, _)| rule)
}

/// Loads, then proves what was loaded: a refused proof drops the loaded value before
/// anything can use it, and nothing is proved about a load that failed.
fn load_proved<L>(
    load: impl FnOnce() -> Result<L>,
    prove: impl FnOnce() -> Result<MappingRule>,
) -> Result<(L, MappingRule)> {
    let loaded = load()?;
    let rule = prove()?;
    Ok((loaded, rule))
}

const RANGE_BYTES: usize = 16 * 1024 * 1024;
const RANGE_ENTRIES: usize = 100_000;

/// One range's pages; `next(begin)` reads the continuation. Every page, first and terminal
/// included, must keep cumulative key+value bytes and entries in budget before it is kept.
fn read_pages(
    (mut page, mut more): (KeyValues, bool),
    mut next: impl FnMut(&[u8]) -> Result<(KeyValues, bool)>,
) -> Result<KeyValues> {
    let mut out = KeyValues::new();
    let mut bytes = 0usize;
    loop {
        let total = page.iter().try_fold(bytes, |sum, (k, v)| {
            sum.checked_add(k.len())?.checked_add(v.len())
        });
        let entries = out.len().checked_add(page.len());
        match (total, entries) {
            (Some(total), Some(entries)) if total <= RANGE_BYTES && entries <= RANGE_ENTRIES => {
                bytes = total;
            }
            _ => {
                return Err(Error::new(
                    ErrorKind::Operation,
                    "stored prior image exceeds read bound",
                ));
            }
        }
        out.extend(page);
        if !more {
            return Ok(out);
        }
        let Some((last, _)) = out.last() else {
            return Err(Error::new(
                ErrorKind::Operation,
                "empty nonterminal FDB range",
            ));
        };
        let mut begin = last.clone();
        begin.push(0);
        (page, more) = next(&begin)?;
    }
}

fn checked(code: Code) -> Result<()> {
    if code == 0 {
        Ok(())
    } else {
        Err(Error {
            kind: if (1000..1500).contains(&code) {
                ErrorKind::Transient
            } else {
                ErrorKind::Operation
            },
            message: format!("FoundationDB error {code}"),
            fdb_code: Some(code),
        })
    }
}

impl Api {
    /// Loads the pinned artifact and proves, before any C API call, that what the
    /// process mapped is that artifact.
    fn load(path: &Path) -> Result<(Self, MappingRule)> {
        let unreadable = || Error::new(ErrorKind::Config, "cannot read explicit libfdb_c file");
        // The identity is the hashed file's own: taken from the same open handle.
        let mut file = fs::File::open(path).map_err(|_| unreadable())?;
        let artifact = FileIdentity::of(&file.metadata().map_err(|_| unreadable())?);
        let mut wire = Vec::new();
        file.read_to_end(&mut wire).map_err(|_| unreadable())?;
        if hex::encode(Sha256::digest(&wire)) != CLIENT_SHA256 {
            return Err(Error::new(
                ErrorKind::Config,
                "libfdb_c SHA256 does not match MODULE.bazel 7.3.77 pin",
            ));
        }
        // Only an explicitly named, hash-verified local artifact is executable.
        let (library, rule) = load_proved(
            || {
                let library = unsafe { Library::new(path) };
                library.map_err(|_| {
                    Error::new(
                        ErrorKind::Config,
                        "cannot load pinned libfdb_c or its system dependencies",
                    )
                })
            },
            || {
                // dlopen named the path again: it must still be the hashed, pinned file.
                let now = fs::metadata(path).map_err(|_| mapping_error(NOT_THE_ARTIFACT))?;
                let again = fs::read(path).map_err(|_| mapping_error(NOT_THE_ARTIFACT))?;
                if FileIdentity::of(&now) != artifact
                    || hex::encode(Sha256::digest(&again)) != CLIENT_SHA256
                {
                    return Err(mapping_error(NOT_THE_ARTIFACT));
                }
                prove_mapped(|p| fs::read_to_string(p), artifact)
            },
        )?;
        macro_rules! symbol {
            ($name:literal) => {
                unsafe {
                    *library.get(concat!($name, "\0").as_bytes()).map_err(|_| {
                        Error::new(ErrorKind::Config, concat!("missing C API symbol ", $name))
                    })?
                }
            };
        }
        let api = Self {
            select: symbol!("fdb_select_api_version_impl"),
            network_option: symbol!("fdb_network_set_option"),
            setup: symbol!("fdb_setup_network"),
            run: symbol!("fdb_run_network"),
            stop: symbol!("fdb_stop_network"),
            open: symbol!("fdb_create_database"),
            close: symbol!("fdb_database_destroy"),
            create: symbol!("fdb_database_create_transaction"),
            destroy: symbol!("fdb_transaction_destroy"),
            cancel: symbol!("fdb_transaction_cancel"),
            option: symbol!("fdb_transaction_set_option"),
            get: symbol!("fdb_transaction_get"),
            range: symbol!("fdb_transaction_get_range"),
            set: symbol!("fdb_transaction_set"),
            clear: symbol!("fdb_transaction_clear"),
            size: symbol!("fdb_transaction_get_approximate_size"),
            commit: symbol!("fdb_transaction_commit"),
            on_error: symbol!("fdb_transaction_on_error"),
            future_destroy: symbol!("fdb_future_destroy"),
            future_callback: symbol!("fdb_future_set_callback"),
            future_error: symbol!("fdb_future_get_error"),
            future_value: symbol!("fdb_future_get_value"),
            future_kvs: symbol!("fdb_future_get_keyvalue_array"),
            future_int64: symbol!("fdb_future_get_int64"),
            _library: library,
        };
        Ok((api, rule))
    }
}

struct Runtime {
    api: Arc<Api>,
    exit: Arc<AtomicI32>,
    network: Option<thread::JoinHandle<Code>>,
}
impl Drop for Runtime {
    fn drop(&mut self) {
        unsafe {
            (self.api.stop)();
        }
        if let Some(thread) = self.network.take() {
            let _ = thread.join();
        }
    }
}

pub struct NativeDatabase {
    runtime: Arc<Runtime>,
    handle: usize,
    /// This open's own proof of the mapped client, never another open's.
    proof: MappedClientProof,
}
// libfdb_c databases are documented thread-safe; all transaction handles remain
// thread-local and futures copy their borrowed C data before destruction.
unsafe impl Send for NativeDatabase {}
unsafe impl Sync for NativeDatabase {}

impl NativeDatabase {
    /// Opening starts the native client's one-shot network thread, but never
    /// starts an FDB server. Call only against an explicitly admitted cluster.
    pub fn open(client: &Path, cluster_file: &Path) -> Result<Self> {
        Self::open_with(client, cluster_file, |_| {})
    }

    /// `adapt` sees the loaded API before network setup; production passes a no-op.
    fn open_with(client: &Path, cluster_file: &Path, adapt: impl FnOnce(&mut Api)) -> Result<Self> {
        refuse_network_option_env(std::env::vars_os())?;
        if !client.is_absolute() || !cluster_file.is_absolute() {
            return Err(Error::new(
                ErrorKind::Config,
                "absolute explicit client and cluster paths required",
            ));
        }
        let cluster = CString::new(cluster_file.as_os_str().as_encoded_bytes())
            .map_err(|_| Error::new(ErrorKind::Config, "invalid cluster path"))?;
        // The mapped-client proof precedes the first C API call and every global change.
        let (mut api, rule) = Api::load(client)?;
        adapt(&mut api);
        let api = Arc::new(api);
        if NETWORK_STARTED.swap(true, Ordering::AcqRel) {
            return Err(Error::new(
                ErrorKind::Operation,
                "native runtime is one-shot; reuse the open database",
            ));
        }
        let _ = LOADED_API.set(api.clone());
        unsafe {
            start_network(api.select, api.network_option, api.setup)?;
        }
        let run_api = api.clone();
        let exit = Arc::new(AtomicI32::new(NETWORK_RUNNING));
        let thread_exit = exit.clone();
        let network = match thread::Builder::new()
            .name("fdb-native-network".into())
            .spawn(move || {
                let code = unsafe { (run_api.run)() };
                thread_exit.store(code, Ordering::Release);
                code
            }) {
            Ok(network) => network,
            Err(_) => {
                unsafe {
                    (api.stop)();
                }
                return Err(Error::new(
                    ErrorKind::Operation,
                    "cannot create FDB network thread",
                ));
            }
        };
        let runtime = Arc::new(Runtime {
            api,
            exit,
            network: Some(network),
        });
        let mut handle = ptr::null_mut();
        unsafe {
            checked((runtime.api.open)(cluster.as_ptr(), &mut handle))?;
        }
        Ok(Self {
            runtime,
            handle: handle as usize,
            proof: MappedClientProof { rule },
        })
    }

    /// The proof that this database's loaded libfdb_c is the sealed artifact.
    pub fn mapping_proof(&self) -> MappedClientProof {
        self.proof
    }
}
impl Drop for NativeDatabase {
    fn drop(&mut self) {
        unsafe {
            (self.runtime.api.close)(self.handle as Handle);
        }
    }
}
impl Database for NativeDatabase {
    fn transaction(&self) -> Result<Box<dyn Transaction + '_>> {
        network_check(&self.runtime.exit)?;
        let mut handle = ptr::null_mut();
        unsafe {
            checked((self.runtime.api.create)(
                self.handle as Handle,
                &mut handle,
            ))?;
        }
        let tx = NativeTransaction {
            api: &self.runtime.api,
            exit: &self.runtime.exit,
            handle,
        };
        // Set once on the SAME handle: libfdb_c OnError preserves the timeout
        // across retries. No outer loop recreates a timed-out transaction.
        for (option, value) in [(500, 10_000i64), (501, -1i64)] {
            unsafe {
                checked((tx.api.option)(
                    handle,
                    option,
                    value.to_le_bytes().as_ptr(),
                    8,
                ))?;
            }
        }
        Ok(Box::new(tx))
    }
}

struct NativeTransaction<'a> {
    api: &'a Api,
    exit: &'a AtomicI32,
    handle: Handle,
}
impl Drop for NativeTransaction<'_> {
    fn drop(&mut self) {
        unsafe {
            (self.api.cancel)(self.handle);
            (self.api.destroy)(self.handle);
        }
    }
}
struct Future<'a> {
    api: &'a Api,
    handle: Handle,
}
impl Drop for Future<'_> {
    fn drop(&mut self) {
        unsafe {
            (self.api.future_destroy)(self.handle);
        }
    }
}
static CALLBACKS: OnceLock<Mutex<HashMap<usize, mpsc::SyncSender<()>>>> = OnceLock::new();
static NEXT_CALLBACK: AtomicUsize = AtomicUsize::new(1);
struct Registration {
    token: usize,
    receiver: mpsc::Receiver<()>,
}
impl Registration {
    fn new() -> Result<Self> {
        let token = NEXT_CALLBACK
            .try_update(Ordering::AcqRel, Ordering::Acquire, |n| n.checked_add(1))
            .map_err(|_| Error::new(ErrorKind::Operation, "native callback token exhausted"))?;
        let (sender, receiver) = mpsc::sync_channel(1);
        CALLBACKS
            .get_or_init(|| Mutex::new(HashMap::new()))
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .insert(token, sender);
        Ok(Self { token, receiver })
    }
}
impl Drop for Registration {
    fn drop(&mut self) {
        CALLBACKS
            .get()
            .unwrap()
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .remove(&self.token);
    }
}
unsafe extern "C" fn ready(_future: Handle, data: *mut c_void) {
    // Userdata is a never-reused opaque token, not a dereferenced allocation.
    // Late callbacks after cancellation/network failure safely find no entry.
    if let Some(registry) = CALLBACKS.get() {
        let sender = registry
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .remove(&(data as usize));
        if let Some(sender) = sender {
            let _ = sender.send(());
        }
    }
}
fn network_check(exit: &AtomicI32) -> Result<()> {
    let code = exit.load(Ordering::Acquire);
    if code == NETWORK_RUNNING {
        Ok(())
    } else {
        Err(Error::new(
            ErrorKind::Operation,
            format!("native network loop stopped: {code}"),
        ))
    }
}

fn wait_ready(
    receiver: &mpsc::Receiver<()>,
    ctx: &Context,
    exit: &AtomicI32,
    mut cancel: impl FnMut(),
) -> Result<()> {
    loop {
        // Cancellation may not complete an uninitialized multiversion future.
        // The owner releases that future/token; a late callback is a safe no-op.
        network_check(exit)?;
        if let Err(err) = ctx.check() {
            cancel();
            return Err(err);
        }
        match receiver.recv_timeout(Duration::from_millis(10)) {
            Ok(()) => return ctx.check(),
            Err(mpsc::RecvTimeoutError::Timeout) => continue,
            Err(_) => {
                return Err(Error::new(
                    ErrorKind::Operation,
                    "FDB callback disconnected",
                ));
            }
        }
    }
}

impl NativeTransaction<'_> {
    fn future(&self, handle: Handle) -> Future<'_> {
        Future {
            api: self.api,
            handle,
        }
    }
    fn wait(&self, future: &Future<'_>, ctx: &Context) -> Result<()> {
        let registration = Registration::new()?;
        let code = unsafe {
            (self.api.future_callback)(future.handle, ready, registration.token as *mut c_void)
        };
        checked(code)?;
        wait_ready(&registration.receiver, ctx, self.exit, || unsafe {
            (self.api.cancel)(self.handle);
        })?;
        unsafe { checked((self.api.future_error)(future.handle)) }
    }
    fn range_future(&self, begin: &[u8], end: &[u8]) -> Future<'_> {
        // FIRST_GREATER_OR_EQUAL selectors, WANT_ALL streaming, serializable.
        self.future(unsafe {
            (self.api.range)(
                self.handle,
                begin.as_ptr(),
                begin.len() as i32,
                0,
                1,
                end.as_ptr(),
                end.len() as i32,
                0,
                1,
                0,
                0,
                -2,
                1,
                0,
                0,
            )
        })
    }
    fn collect_range(&self, future: Future<'_>, ctx: &Context) -> Result<(KeyValues, bool)> {
        self.wait(&future, ctx)?;
        let mut kvs = ptr::null();
        let mut count = 0;
        let mut more = 0;
        unsafe {
            checked((self.api.future_kvs)(
                future.handle,
                &mut kvs,
                &mut count,
                &mut more,
            ))?;
        }
        if !(0..=1_000_000).contains(&count) {
            return Err(Error::new(ErrorKind::Operation, "invalid FDB range count"));
        }
        let mut out = Vec::with_capacity(count as usize);
        for i in 0..count as usize {
            let kv = unsafe { kvs.add(i).read_unaligned() };
            if kv.key_length < 0 || kv.value_length < 0 {
                return Err(Error::new(
                    ErrorKind::Operation,
                    "invalid FDB range lengths",
                ));
            }
            // The packed C array and its data are valid until Future is dropped.
            let key =
                unsafe { std::slice::from_raw_parts(kv.key, kv.key_length as usize) }.to_vec();
            let value = if kv.value_length == 0 {
                vec![]
            } else {
                unsafe { std::slice::from_raw_parts(kv.value, kv.value_length as usize) }.to_vec()
            };
            out.push((key, value));
        }
        Ok((out, more != 0))
    }
}
impl Transaction for NativeTransaction<'_> {
    fn get(&mut self, key: &[u8], ctx: &Context) -> Result<Option<Vec<u8>>> {
        let f =
            self.future(unsafe { (self.api.get)(self.handle, key.as_ptr(), key.len() as i32, 0) });
        self.wait(&f, ctx)?;
        let mut present = 0;
        let mut value = ptr::null();
        let mut len = 0;
        unsafe {
            checked((self.api.future_value)(
                f.handle,
                &mut present,
                &mut value,
                &mut len,
            ))?;
        }
        if present == 0 {
            return Ok(None);
        }
        if len < 0 {
            return Err(Error::new(ErrorKind::Operation, "invalid FDB value length"));
        }
        Ok(Some(if len == 0 {
            vec![]
        } else {
            unsafe { std::slice::from_raw_parts(value, len as usize) }.to_vec()
        }))
    }
    fn ranges(&mut self, ranges: &[(Vec<u8>, Vec<u8>)], ctx: &Context) -> Result<Vec<KeyValues>> {
        // The first read of every old image is dispatched before any waits.
        let futures = ranges
            .iter()
            .map(|(b, e)| self.range_future(b, e))
            .collect::<Vec<_>>();
        let mut result = Vec::with_capacity(ranges.len());
        for ((_, end), future) in ranges.iter().zip(futures) {
            let first = self.collect_range(future, ctx)?;
            result.push(read_pages(first, |begin| {
                self.collect_range(self.range_future(begin, end), ctx)
            })?);
        }
        Ok(result)
    }
    fn set(&mut self, key: &[u8], value: &[u8]) {
        unsafe {
            (self.api.set)(
                self.handle,
                key.as_ptr(),
                key.len() as i32,
                value.as_ptr(),
                value.len() as i32,
            );
        }
    }
    fn clear(&mut self, key: &[u8]) {
        unsafe {
            (self.api.clear)(self.handle, key.as_ptr(), key.len() as i32);
        }
    }
    fn approximate_size(&mut self, ctx: &Context) -> Result<i64> {
        let f = self.future(unsafe { (self.api.size)(self.handle) });
        self.wait(&f, ctx)?;
        let mut size = 0;
        unsafe {
            checked((self.api.future_int64)(f.handle, &mut size))?;
        }
        if size < 0 {
            return Err(Error::new(
                ErrorKind::Operation,
                "negative FDB size estimate",
            ));
        }
        Ok(size)
    }
    fn commit(&mut self, ctx: &Context) -> Result<()> {
        self.wait(&self.future(unsafe { (self.api.commit)(self.handle) }), ctx)
    }
    fn on_error(&mut self, code: i32, ctx: &Context) -> Result<()> {
        self.wait(
            &self.future(unsafe { (self.api.on_error)(self.handle, code) }),
            ctx,
        )
    }
}

/// Test-only C-boundary interceptor: forwards the real libfdb_c calls and, only after a
/// commit future really reports success, can report literal 1021 once for that future.
#[cfg(test)]
#[allow(
    dead_code,
    reason = "entry points used only by the native proof driver"
)]
pub(crate) mod fault {
    use super::*;
    use std::{
        collections::HashSet,
        sync::{LazyLock, MutexGuard},
    };

    #[derive(Clone, Copy)]
    pub(super) struct Real {
        pub create: unsafe extern "C" fn(Handle, *mut Handle) -> Code,
        pub option: unsafe extern "C" fn(Handle, i32, *const u8, i32) -> Code,
        pub commit: unsafe extern "C" fn(Handle) -> Handle,
        pub future_error: unsafe extern "C" fn(Handle) -> Code,
        pub future_destroy: unsafe extern "C" fn(Handle),
        pub on_error: unsafe extern "C" fn(Handle, Code) -> Handle,
    }

    /// Calls seen at the boundary; handles are opaque addresses.
    #[derive(Clone, Debug, Default)]
    pub(crate) struct Counters {
        pub created: Vec<usize>,
        pub options: Vec<(usize, i32, Vec<u8>)>,
        pub commits: Vec<usize>,
        pub real_commit_successes: usize,
        pub exposed_1021: usize,
        pub on_errors: Vec<(usize, Code)>,
    }

    #[derive(Default)]
    struct State {
        counters: Counters,
        commit_futures: HashSet<usize>,
        expose: usize,
    }
    static REAL: OnceLock<Real> = OnceLock::new();
    static STATE: LazyLock<Mutex<State>> = LazyLock::new(Default::default);

    fn state() -> MutexGuard<'static, State> {
        STATE.lock().unwrap_or_else(|p| p.into_inner())
    }
    fn real() -> Real {
        *REAL.get().expect("interceptor installed")
    }

    pub(super) fn install_real(real: Real) {
        assert!(REAL.set(real).is_ok(), "one interceptor per process");
    }

    /// Routes the loaded API through the interceptor; every other symbol stays real.
    fn install(api: &mut Api) {
        install_real(Real {
            create: api.create,
            option: api.option,
            commit: api.commit,
            future_error: api.future_error,
            future_destroy: api.future_destroy,
            on_error: api.on_error,
        });
        api.create = create;
        api.option = option;
        api.commit = commit;
        api.future_error = future_error;
        api.future_destroy = future_destroy;
        api.on_error = on_error;
    }

    impl NativeDatabase {
        pub(crate) fn open_intercepted(client: &Path, cluster_file: &Path) -> Result<Self> {
            Self::open_with(client, cluster_file, install)
        }
    }

    pub(crate) fn expose_commit_unknown_once() {
        state().expose = 1;
    }
    pub(crate) fn counters() -> Counters {
        state().counters.clone()
    }

    pub(super) unsafe extern "C" fn create(db: Handle, out: *mut Handle) -> Code {
        let code = unsafe { (real().create)(db, out) };
        if code == 0 {
            state().counters.created.push(unsafe { *out } as usize);
        }
        code
    }
    pub(super) unsafe extern "C" fn option(
        tx: Handle,
        option: i32,
        value: *const u8,
        length: i32,
    ) -> Code {
        let bytes = if value.is_null() || length <= 0 {
            Vec::new()
        } else {
            unsafe { std::slice::from_raw_parts(value, length as usize) }.to_vec()
        };
        state().counters.options.push((tx as usize, option, bytes));
        unsafe { (real().option)(tx, option, value, length) }
    }
    pub(super) unsafe extern "C" fn commit(tx: Handle) -> Handle {
        let future = unsafe { (real().commit)(tx) };
        let mut state = state();
        state.counters.commits.push(tx as usize);
        state.commit_futures.insert(future as usize);
        future
    }
    pub(super) unsafe extern "C" fn future_error(future: Handle) -> Code {
        let code = unsafe { (real().future_error)(future) };
        let mut state = state();
        if code == 0 && state.commit_futures.contains(&(future as usize)) {
            state.counters.real_commit_successes += 1;
            if state.expose > 0 {
                state.expose -= 1;
                state.counters.exposed_1021 += 1;
                return 1021;
            }
        }
        code
    }
    pub(super) unsafe extern "C" fn future_destroy(future: Handle) {
        state().commit_futures.remove(&(future as usize));
        unsafe { (real().future_destroy)(future) }
    }
    pub(super) unsafe extern "C" fn on_error(tx: Handle, code: Code) -> Handle {
        state().counters.on_errors.push((tx as usize, code));
        unsafe { (real().on_error)(tx, code) }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{os::unix::ffi::OsStringExt, process::Command};

    const SENTINEL: &[u8] = b"sentinel-7f3a";

    fn os(bytes: &[u8]) -> OsString {
        OsString::from_vec(bytes.to_vec())
    }

    fn with_sentinel(prefix: &[u8], suffix: &[u8]) -> OsString {
        os(&[prefix, SENTINEL, suffix].concat())
    }

    /// (name, key, value, refused): one entry per variable class and unrelated control.
    fn classes() -> Vec<(&'static str, OsString, OsString, bool)> {
        vec![
            (
                "normal",
                os(b"FDB_NETWORK_OPTION_TRACE_ENABLE"),
                with_sentinel(b"/", b""),
                true,
            ),
            (
                "empty value",
                os(b"FDB_NETWORK_OPTION_TRACE_LOG_GROUP"),
                os(b""),
                true,
            ),
            (
                "non-UTF-8 value",
                os(b"FDB_NETWORK_OPTION_KNOB"),
                with_sentinel(b"\xff", b"\xfe"),
                true,
            ),
            (
                "non-UTF-8 key",
                os(b"FDB_NETWORK_OPTION_\xffKNOB"),
                with_sentinel(b"", b""),
                true,
            ),
            (
                "bare prefix",
                os(b"FDB_NETWORK_OPTION_"),
                with_sentinel(b"", b""),
                true,
            ),
            (
                "near-miss key",
                os(b"FDB_NETWORK_OPTIONS_TRACE_ENABLE"),
                with_sentinel(b"", b""),
                false,
            ),
            (
                "lowercase key",
                os(b"fdb_network_option_trace_enable"),
                with_sentinel(b"", b""),
                false,
            ),
            (
                "embedded prefix",
                os(b"XFDB_NETWORK_OPTION_TRACE_ENABLE"),
                with_sentinel(b"", b""),
                false,
            ),
            (
                "prefix in value",
                os(b"UNRELATED"),
                os(b"FDB_NETWORK_OPTION_TRACE_ENABLE"),
                false,
            ),
            (
                "non-UTF-8 unrelated key",
                os(b"\xffFDB_NETWORK_OPTION_KNOB"),
                with_sentinel(b"", b""),
                false,
            ),
        ]
    }

    #[test]
    fn network_option_variables_are_refused_by_raw_key_prefix() {
        let other = || (os(b"PATH"), os(b"/usr/bin"));
        assert!(refuse_network_option_env(Vec::<(OsString, OsString)>::new()).is_ok());
        for (name, key, value, refused) in classes() {
            let result = refuse_network_option_env([other(), (key, value), other()]);
            if refused {
                let err = result.unwrap_err();
                assert_eq!(err.kind, ErrorKind::Config, "{name}");
                assert_eq!(err.message, ENV_OPTION_REFUSED, "{name}");
            } else {
                assert!(result.is_ok(), "{name}");
            }
        }
    }

    static CALLS: Mutex<Vec<String>> = Mutex::new(Vec::new());
    extern "C" fn fake_select(runtime: i32, header: i32) -> Code {
        CALLS
            .lock()
            .unwrap()
            .push(format!("select {runtime} {header}"));
        0
    }
    extern "C" fn fake_option(option: i32, value: *const u8, length: i32) -> Code {
        let null = value.is_null();
        CALLS
            .lock()
            .unwrap()
            .push(format!("option {option} null={null} {length}"));
        0
    }
    extern "C" fn failing_option(option: i32, value: *const u8, length: i32) -> Code {
        fake_option(option, value, length);
        2006
    }
    extern "C" fn fake_setup() -> Code {
        CALLS.lock().unwrap().push("setup".into());
        0
    }

    #[test]
    fn start_network_disables_multi_version_client_after_select_before_setup() {
        unsafe { start_network(fake_select, fake_option, fake_setup) }.unwrap();
        assert_eq!(
            std::mem::take(&mut *CALLS.lock().unwrap()),
            ["select 730 730", "option 60 null=true 0", "setup"]
        );
        let err = unsafe { start_network(fake_select, failing_option, fake_setup) }.unwrap_err();
        assert_eq!(err.fdb_code, Some(2006));
        assert_eq!(
            std::mem::take(&mut *CALLS.lock().unwrap()),
            ["select 730 730", "option 60 null=true 0"],
            "a failed option must not set up the network"
        );
    }

    /// Hands out ascending 4-byte keys across a scenario's pages.
    struct Keys(u32);
    impl Keys {
        /// `entries` rows totalling exactly `bytes` key+value bytes, values at most 100000.
        fn page(&mut self, entries: usize, bytes: usize, more: bool) -> (KeyValues, bool) {
            let mut values = bytes - 4 * entries;
            let page: KeyValues = (0..entries)
                .map(|_| {
                    self.0 += 1;
                    let len = values.min(100_000);
                    values -= len;
                    (self.0.to_be_bytes().to_vec(), vec![0xab; len])
                })
                .collect();
            assert_eq!(values, 0, "too few entries for {bytes} bytes");
            let counted: usize = page.iter().map(|(k, v)| k.len() + v.len()).sum();
            assert_eq!((page.len(), counted), (entries, bytes));
            (page, more)
        }
    }
    fn byte_page(keys: &mut Keys, bytes: usize, more: bool) -> (KeyValues, bool) {
        keys.page((bytes / 100_000 + 1).min(bytes / 4), bytes, more)
    }
    fn entry_page(keys: &mut Keys, entries: usize, more: bool) -> (KeyValues, bool) {
        keys.page(entries, 4 * entries, more)
    }

    /// Runs `read_pages` over scripted pages; returns its result and the continuation count.
    fn drive(pages: Vec<(KeyValues, bool)>) -> (Result<KeyValues>, usize) {
        let mut pages = std::collections::VecDeque::from(pages);
        let first = pages.pop_front().unwrap();
        let mut last = first.0.last().map(|(k, _)| k.clone());
        let mut fetched = 0;
        let result = read_pages(first, |begin| {
            fetched += 1;
            let mut want = last.clone().expect("continuation after a nonempty page");
            want.push(0);
            assert_eq!(
                begin,
                want.as_slice(),
                "continuation begins after the last key"
            );
            let page = pages.pop_front().expect("fetch after the terminal page");
            if let Some((k, _)) = page.0.last() {
                last = Some(k.clone());
            }
            Ok(page)
        });
        (result, fetched)
    }

    /// Exact/+1 at each page position plus cumulative controls; `small` is a minimal page.
    fn check_range_bound(
        name: &str,
        limit: usize,
        small: usize,
        page: fn(&mut Keys, usize, bool) -> (KeyValues, bool),
    ) {
        let (half, third) = (limit / 2, limit / 3);
        let scenarios: [(&str, Vec<(usize, bool)>, Option<usize>); 12] = [
            ("first terminal exact", vec![(limit, false)], None),
            ("first terminal +1", vec![(limit + 1, false)], Some(0)),
            (
                "first nonterminal exact",
                vec![(limit, true), (0, false)],
                None,
            ),
            (
                "first nonterminal +1",
                vec![(limit + 1, true), (small, false)],
                Some(0),
            ),
            (
                "intermediate exact",
                vec![(small, true), (limit - small, true), (0, false)],
                None,
            ),
            (
                "intermediate +1",
                vec![(small, true), (limit - small + 1, true), (small, false)],
                Some(1),
            ),
            (
                "terminal continuation exact",
                vec![(small, true), (limit - small, false)],
                None,
            ),
            (
                "terminal continuation +1",
                vec![(small, true), (limit - small + 1, false)],
                Some(1),
            ),
            (
                "pages within, combined exact",
                vec![(half, true), (limit - half, false)],
                None,
            ),
            (
                "pages within, combined +1",
                vec![(half, true), (limit - half + 1, false)],
                Some(1),
            ),
            (
                "three pages, combined exact",
                vec![(third, true), (third, true), (limit - 2 * third, false)],
                None,
            ),
            (
                "three pages, combined +1 nonterminal",
                vec![
                    (third, true),
                    (third, true),
                    (limit - 2 * third + 1, true),
                    (small, false),
                ],
                Some(2),
            ),
        ];
        for (label, sizes, reject) in scenarios {
            let mut keys = Keys(0);
            let pages: Vec<_> = sizes
                .iter()
                .map(|&(n, more)| page(&mut keys, n, more))
                .collect();
            let expected: KeyValues = pages.iter().flat_map(|(p, _)| p.clone()).collect();
            let (result, fetched) = drive(pages);
            match reject {
                None => {
                    assert_eq!(fetched, sizes.len() - 1, "{name} {label}");
                    assert!(result.unwrap() == expected, "{name} {label}: rows or order");
                }
                Some(at) => {
                    assert_eq!(fetched, at, "{name} {label}: fetched after rejection");
                    let err = result.unwrap_err();
                    assert_eq!(err.kind, ErrorKind::Operation, "{name} {label}");
                    assert_eq!(err.message, "stored prior image exceeds read bound");
                }
            }
        }
    }

    #[test]
    fn range_byte_budget_applies_to_every_page() {
        // Independent literal: 16 MiB of key+value bytes per range.
        check_range_bound("bytes", 16 * 1024 * 1024, 4, byte_page);
    }

    #[test]
    fn range_entry_budget_applies_to_every_page() {
        // Independent literal: 100000 entries per range, under 1 MiB of bytes.
        check_range_bound("entries", 100_000, 1, entry_page);
    }

    #[test]
    fn empty_nonterminal_first_page_is_rejected_without_fetching() {
        let (result, fetched) = drive(vec![(Vec::new(), true)]);
        assert_eq!(result.unwrap_err().message, "empty nonterminal FDB range");
        assert_eq!(fetched, 0);
    }

    static FAKE_CODE: AtomicI32 = AtomicI32::new(0);
    static FAKE_FUTURE: AtomicUsize = AtomicUsize::new(0x1000);
    extern "C" fn fake_create(_: Handle, out: *mut Handle) -> Code {
        unsafe { *out = 0x70 as Handle };
        0
    }
    extern "C" fn fake_tx_option(_: Handle, _: i32, _: *const u8, _: i32) -> Code {
        0
    }
    extern "C" fn fake_commit(_: Handle) -> Handle {
        FAKE_FUTURE.fetch_add(1, Ordering::Relaxed) as Handle
    }
    extern "C" fn fake_future_error(_: Handle) -> Code {
        FAKE_CODE.load(Ordering::Relaxed)
    }
    extern "C" fn fake_destroy(_: Handle) {}
    extern "C" fn fake_on_error(_: Handle, _: Code) -> Handle {
        0x90 as Handle
    }

    #[test]
    fn interceptor_exposes_1021_once_and_only_after_a_real_commit_success() {
        fault::install_real(fault::Real {
            create: fake_create,
            option: fake_tx_option,
            commit: fake_commit,
            future_error: fake_future_error,
            future_destroy: fake_destroy,
            on_error: fake_on_error,
        });
        fault::expose_commit_unknown_once();
        let mut tx = ptr::null_mut();
        let ten_seconds = 10_000i64.to_le_bytes();
        unsafe {
            assert_eq!(fault::create(ptr::null_mut(), &mut tx), 0);
            assert_eq!(fault::option(tx, 500, ten_seconds.as_ptr(), 8), 0);
            // A real commit failure passes through and keeps the exposure armed.
            FAKE_CODE.store(1020, Ordering::Relaxed);
            let failed = fault::commit(tx);
            assert_eq!(fault::future_error(failed), 1020);
            fault::future_destroy(failed);
            // A non-commit future's success is never rewritten.
            FAKE_CODE.store(0, Ordering::Relaxed);
            assert_eq!(fault::future_error(0x5 as Handle), 0);
            let applied = fault::commit(tx);
            assert_eq!(fault::future_error(applied), 1021);
            fault::future_destroy(applied);
            assert_eq!(fault::on_error(tx, 1021), 0x90 as Handle);
            let replay = fault::commit(tx);
            assert_eq!(fault::future_error(replay), 0);
            fault::future_destroy(replay);
        }
        let c = fault::counters();
        assert_eq!(c.created, [0x70]);
        assert_eq!(c.options, [(0x70, 500, ten_seconds.to_vec())]);
        assert_eq!(c.commits, [0x70; 3]);
        assert_eq!((c.real_commit_successes, c.exposed_1021), (2, 1));
        assert_eq!(c.on_errors, [(0x70, 1021)]);
    }

    const CHILD_EXPECT: &str = "OTEL_RUST_NATIVE_ENV_EXPECT";

    /// The subprocess side: `open` fails as `expect` says, before any load or global mutation.
    fn assert_open_outcome(expect: &str) {
        let err = match NativeDatabase::open(
            Path::new("/nonexistent-otel-rust/libfdb_c.so"),
            Path::new("/nonexistent-otel-rust/fdb.cluster"),
        ) {
            Ok(_) => panic!("open must fail"),
            Err(err) => err,
        };
        let message = match expect {
            "refuse" => ENV_OPTION_REFUSED,
            "load" => "cannot read explicit libfdb_c file",
            other => panic!("unknown expectation {other}"),
        };
        assert_eq!(err.kind, ErrorKind::Config);
        assert_eq!(err.message, message);
        assert!(!NETWORK_STARTED.load(Ordering::Acquire));
        assert!(LOADED_API.get().is_none());
    }

    #[test]
    fn open_checks_environment_before_loading_the_client() {
        // A spawned run of this same test carries the marker and checks `open` itself.
        if let Ok(expect) = std::env::var(CHILD_EXPECT) {
            assert_open_outcome(&expect);
            return;
        }
        let exe = std::env::current_exe().unwrap();
        let mut runs = vec![("no variables", None, false)];
        runs.extend(
            classes()
                .into_iter()
                .map(|(name, key, value, refused)| (name, Some((key, value)), refused)),
        );
        assert_eq!(runs.len(), 11);
        for (name, var, refused) in runs {
            let mut child = Command::new(&exe);
            child
                .args([
                    "--exact",
                    "native::tests::open_checks_environment_before_loading_the_client",
                    "--nocapture",
                    "--test-threads=1",
                ])
                .env_clear()
                .env(CHILD_EXPECT, if refused { "refuse" } else { "load" });
            if let Some((key, value)) = var {
                child.env(key, value);
            }
            let out = child.output().unwrap();
            let printed = [out.stdout.as_slice(), out.stderr.as_slice()].concat();
            assert!(out.status.success(), "{name}: child failed");
            assert!(
                String::from_utf8_lossy(&out.stdout).contains("1 passed"),
                "{name}: child test did not run"
            );
            assert!(
                !printed.windows(SENTINEL.len()).any(|w| w == SENTINEL),
                "{name}: environment value printed"
            );
        }
    }

    #[test]
    fn dead_network_does_not_wait_for_an_unreachable_callback() {
        let (_sender, receiver) = mpsc::sync_channel(1);
        let exit = AtomicI32::new(4000);
        let ctx =
            Context::default().with_deadline(std::time::Instant::now() + Duration::from_secs(5));
        assert_eq!(
            wait_ready(&receiver, &ctx, &exit, || panic!("dead loop cannot cancel"))
                .unwrap_err()
                .kind,
            ErrorKind::Operation
        );
    }
    #[test]
    fn late_callback_after_registration_drop_is_a_noop() {
        let registration = Registration::new().unwrap();
        let token = registration.token;
        drop(registration);
        unsafe {
            ready(ptr::null_mut(), token as *mut c_void);
        }
        assert!(
            !CALLBACKS
                .get()
                .unwrap()
                .lock()
                .unwrap()
                .contains_key(&token)
        );
    }
    #[test]
    fn registered_callback_delivers_readiness() {
        let registration = Registration::new().unwrap();
        unsafe {
            ready(ptr::null_mut(), registration.token as *mut c_void);
        }
        assert_eq!(registration.receiver.try_recv(), Ok(()));
        unsafe {
            ready(ptr::null_mut(), registration.token as *mut c_void);
        }
        assert_eq!(
            registration.receiver.try_recv(),
            Err(mpsc::TryRecvError::Disconnected)
        );
    }
    #[test]
    fn caller_cancellation_returns_even_if_native_cancel_never_delivers_readiness() {
        let (pending, receiver) = mpsc::sync_channel(1);
        let (canceled, cancellation) = mpsc::sync_channel(1);
        let (finished, result) = mpsc::sync_channel(1);
        let ctx = Context::default();
        let caller = ctx.clone();
        let worker = thread::spawn(move || {
            let exit = AtomicI32::new(NETWORK_RUNNING);
            let outcome = wait_ready(&receiver, &ctx, &exit, || {
                canceled.send(()).unwrap();
            });
            finished.send(outcome).unwrap();
        });
        caller.cancel();
        let cancellation = cancellation.recv_timeout(Duration::from_secs(5));
        let outcome = result.recv_timeout(Duration::from_secs(5));
        // Disconnect only after observing completion, so a broken waiter can
        // terminate during test cleanup without manufacturing native readiness.
        drop(pending);
        worker.join().unwrap();
        cancellation.unwrap();
        assert_eq!(outcome.unwrap().unwrap_err().kind, ErrorKind::Canceled);
    }
    #[test]
    fn cancellation_is_issued_once_when_readiness_races() {
        let (sender, receiver) = mpsc::sync_channel(1);
        let exit = AtomicI32::new(NETWORK_RUNNING);
        let ctx = Context::default();
        ctx.cancel();
        let mut calls = 0;
        assert_eq!(
            wait_ready(&receiver, &ctx, &exit, || {
                calls += 1;
                sender.send(()).unwrap();
            })
            .unwrap_err()
            .kind,
            ErrorKind::Canceled
        );
        assert_eq!(calls, 1);
    }

    // The mapped-client proof. Fixtures are real-format maps and mountinfo text over real
    // temporary files; none of them is a claim about this process's own mappings.

    /// A private, resolved directory of real files, removed on drop.
    struct Dir(std::path::PathBuf);
    impl Dir {
        fn new(name: &str) -> Self {
            static NEXT: AtomicUsize = AtomicUsize::new(0);
            let dir = std::env::temp_dir().join(format!(
                "otel-rust-native-{name}-{}-{}",
                std::process::id(),
                NEXT.fetch_add(1, Ordering::Relaxed)
            ));
            fs::create_dir(&dir).unwrap();
            // The kernel names a mapping by its resolved path, which mount points contain.
            Self(fs::canonicalize(&dir).unwrap())
        }
        fn file(&self, name: &str, bytes: &[u8]) -> String {
            let path = self.0.join(name);
            fs::create_dir_all(path.parent().unwrap()).unwrap();
            fs::write(&path, bytes).unwrap();
            path.to_str().unwrap().to_owned()
        }
        fn path(&self) -> &str {
            self.0.to_str().unwrap()
        }
        fn parent(&self) -> &str {
            self.0.parent().unwrap().to_str().unwrap()
        }
    }
    impl Drop for Dir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    fn identity(path: &str) -> FileIdentity {
        FileIdentity::of(&fs::metadata(path).unwrap())
    }

    /// A real-format maps line: hexadecimal device, decimal inode.
    fn map_line(
        range: &str,
        perms: &str,
        offset: &str,
        dev: Device,
        ino: u64,
        path: &str,
    ) -> String {
        format!(
            "{range} {perms} {offset} {:02x}:{:02x} {ino} {path}",
            dev.major, dev.minor
        )
    }
    fn mapping(dev: Device, ino: u64, path: &str) -> String {
        map_line(
            "7f3a10200000-7f3a10e00000",
            "r-xp",
            "00200000",
            dev,
            ino,
            path,
        )
    }
    /// The three segments a loader maps for one shared object.
    fn segments(dev: Device, ino: u64, path: &str) -> String {
        [
            map_line(
                "7f3a10000000-7f3a10200000",
                "r--p",
                "00000000",
                dev,
                ino,
                path,
            ),
            mapping(dev, ino, path),
            map_line(
                "7f3a10e00000-7f3a10f00000",
                "rw-p",
                "00e00000",
                dev,
                ino,
                path,
            ),
        ]
        .join("\n")
    }
    const LIBC: &str =
        "7f3a20000000-7f3a20028000 r--p 00000000 08:01 1312 /usr/lib/x86_64-linux-gnu/libc.so.6";
    const ROOT_EXT4: &str =
        "1 0 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw,errors=remount-ro\n";
    const PROC: &str =
        "22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw\n";
    const NSFS: &str = "60 25 0:4 net:[4026533080] /run/netns/a rw shared:9 - nsfs nsfs rw\n";

    /// A real-format mountinfo line; mountinfo devices are decimal.
    fn mount(dev: Device, root: &str, point: &str, optional: &str, fstype: &str) -> String {
        format!(
            "36 1 {}:{} {root} {point} rw,relatime{optional} - {fstype} /dev/nvme0n1p2 rw\n",
            dev.major, dev.minor
        )
    }

    /// A superblock device that is not the file's own stat device.
    fn superblock(id: FileIdentity) -> Device {
        let mut sb = Device {
            major: 0,
            minor: 0x1d,
        };
        if sb == stat_device(id.dev) {
            sb.minor += 1;
        }
        sb
    }

    type Verdict = std::result::Result<(MappingRule, usize), String>;

    /// The checker over real stat, with how many mapped pathnames it stat'ed.
    fn verdict(maps: &str, mounts: &str, artifact: FileIdentity) -> (Verdict, usize) {
        let stats = std::cell::Cell::new(0);
        let result = check_mapped_client(maps, mounts, artifact, |p| {
            stats.set(stats.get() + 1);
            fs::metadata(p).ok().map(|m| FileIdentity::of(&m))
        });
        (result.map_err(|e| e.message), stats.get())
    }

    #[test]
    fn stat_device_matches_glibc_vectors() {
        let dev = |major, minor| Device { major, minor };
        for (raw, want) in [
            (0x801, dev(8, 1)),
            (0x10302, dev(259, 2)),
            (0x1231_0345, dev(259, 74565)),
            (1 << 44, dev(4096, 0)),
            (1 << 20, dev(0, 256)),
            (0xffff_f000_0000_0000, dev(0xffff_f000, 0)),
            (0x0000_0fff_fff0_0000, dev(0, 0xffff_ff00)),
            (u64::MAX, dev(0xffff_ffff, 0xffff_ffff)),
        ] {
            assert_eq!(stat_device(raw), want, "{raw:#x}");
        }
        // Maps devices are hexadecimal, mountinfo devices decimal.
        assert_eq!(mapped_identity("00:1d", "7"), Some((dev(0, 29), 7)));
        assert_eq!(
            parse_mountinfo("36 1 0:29 / / rw - btrfs /dev/x rw").unwrap()[0].device,
            dev(0, 29)
        );
    }

    #[test]
    fn mapped_client_populations() {
        let dir = Dir::new("populations");
        let client = dir.file("libfdb_c.so", b"pinned client");
        let id = identity(&client);
        let exact = stat_device(id.dev);
        let three = format!("{}\n{LIBC}\n", segments(exact, id.ino, &client));
        assert_eq!(three.lines().count(), 4);
        assert_eq!(
            verdict(&three, "", id),
            (Ok((MappingRule::ExactStat, 3)), 3)
        );
        let one = mapping(exact, id.ino, &client);
        assert_eq!(verdict(&one, "", id), (Ok((MappingRule::ExactStat, 1)), 1));
        assert_eq!(
            verdict(LIBC, "", id),
            (Err("the pinned native client is not mapped".into()), 0)
        );
        let other = dir.file("other/libfdb_c.so", b"pinned client");
        let other_id = identity(&other);
        assert_ne!(other_id.ino, id.ino);
        let four = format!(
            "{three}{}\n",
            mapping(stat_device(other_id.dev), other_id.ino, &other)
        );
        assert_eq!(verdict(&four, "", id), (Err(NOT_THE_ARTIFACT.into()), 4));
    }

    #[test]
    fn btrfs_mount_population_and_selection() {
        let dir = Dir::new("mounts");
        let client = dir.file("libfdb_c.so", b"pinned client");
        let id = identity(&client);
        let sb = superblock(id);
        let info = format!(
            "{ROOT_EXT4}{PROC}{}{NSFS}",
            mount(sb, "/@home", dir.path(), " shared:2", "btrfs")
        );
        let mounts = parse_mountinfo(&info).unwrap();
        assert_eq!(mounts.len(), 4);
        let winner = longest_containing(&mounts, &client).unwrap();
        assert_eq!(winner.len(), 1);
        assert_eq!(
            (winner[0].point.as_str(), winner[0].fstype.as_str()),
            (dir.path(), "btrfs")
        );
        let maps = mapping(sb, id.ino, &client);
        assert_eq!(
            verdict(&maps, &info, id),
            (Ok((MappingRule::BtrfsSuperblock, 1)), 1)
        );
        let tied = format!(
            "{info}{}",
            mount(sb, "/@home", dir.path(), " shared:5", "btrfs")
        );
        let mounts = parse_mountinfo(&tied).unwrap();
        assert_eq!(mounts.len(), 5);
        assert_eq!(longest_containing(&mounts, &client).unwrap().len(), 2);
        assert_eq!(verdict(&maps, &tied, id).0, Err(NOT_THE_ARTIFACT.into()));
    }

    #[test]
    fn mapped_client_cases() {
        let dir = Dir::new("cases");
        let client = dir.file("libfdb_c.so", b"pinned client");
        let other = dir.file("other/libfdb_c.so", b"pinned client");
        let id = identity(&client);
        let other_id = identity(&other);
        let (exact, sb) = (stat_device(id.dev), superblock(id));
        let d = dir.path();
        let ex1 = mapping(exact, id.ino, &client);
        let sb1 = mapping(sb, id.ino, &client);
        let btrfs = |root: &str, point: &str| mount(sb, root, point, " shared:2", "btrfs");
        let info = format!("{ROOT_EXT4}{PROC}{}{NSFS}", btrfs("/@home", d));
        let raw = |dev: &str, ino: &str, path: &str| {
            format!("7f3a10200000-7f3a10e00000 r-xp 00200000 {dev} {ino} {path}")
        };
        let ino = id.ino.to_string();
        let exact_text = format!("{:02x}:{:02x}", exact.major, exact.minor);
        let malformed = "a native client mapping is malformed";
        let ambiguous = "a mapped native client is deleted or ambiguous";
        let other_fs =
            |dev: Device, fstype: &str| format!("{ROOT_EXT4}{}", mount(dev, "/", d, "", fstype));
        let line = |text: &str| format!("{ROOT_EXT4}{text}\n");
        let cases: Vec<(&str, String, String, std::result::Result<MappingRule, &str>)> = vec![
            // Accepted: exact stat needs no mount evidence; btrfs only with strict proof.
            (
                "exact on btrfs",
                ex1.clone(),
                info.clone(),
                Ok(MappingRule::ExactStat),
            ),
            (
                "exact, no mounts",
                ex1.clone(),
                String::new(),
                Ok(MappingRule::ExactStat),
            ),
            (
                "exact, escaped mounts",
                ex1.clone(),
                format!("{info}31 1 0:40 / /mnt/a\\040b rw - tmpfs t rw\n"),
                Ok(MappingRule::ExactStat),
            ),
            (
                "exact, malformed mounts",
                ex1.clone(),
                "not mountinfo".into(),
                Ok(MappingRule::ExactStat),
            ),
            (
                "btrfs subvolume",
                sb1.clone(),
                info.clone(),
                Ok(MappingRule::BtrfsSuperblock),
            ),
            (
                "btrfs ancestor mount",
                sb1.clone(),
                format!("{ROOT_EXT4}{}", btrfs("/@home", dir.parent())),
                Ok(MappingRule::BtrfsSuperblock),
            ),
            (
                "nested subvolume root",
                sb1.clone(),
                format!("{ROOT_EXT4}{}", btrfs("/@home/.snapshots/1/snapshot", d)),
                Ok(MappingRule::BtrfsSuperblock),
            ),
            (
                "no optional fields",
                sb1.clone(),
                format!("{ROOT_EXT4}{}", mount(sb, "/@home", d, "", "btrfs")),
                Ok(MappingRule::BtrfsSuperblock),
            ),
            (
                "several optional fields",
                sb1.clone(),
                format!(
                    "{ROOT_EXT4}{}",
                    mount(sb, "/@home", d, " shared:2 master:1", "btrfs")
                ),
                Ok(MappingRule::BtrfsSuperblock),
            ),
            (
                "no trailing newline",
                sb1.clone(),
                info.trim_end_matches('\n').into(),
                Ok(MappingRule::BtrfsSuperblock),
            ),
            (
                "the longest of nested btrfs mounts",
                sb1.clone(),
                format!(
                    "{ROOT_EXT4}{}{}",
                    mount(
                        Device {
                            major: 0,
                            minor: 99
                        },
                        "/@",
                        dir.parent(),
                        "",
                        "btrfs"
                    ),
                    btrfs("/@home", d)
                ),
                Ok(MappingRule::BtrfsSuperblock),
            ),
            (
                "unrelated equal mount points",
                sb1.clone(),
                format!(
                    "{info}50 1 8:17 / /mnt/media rw - ext4 /dev/sdb1 rw\n51 1 8:33 / /mnt/media rw - ext4 /dev/sdc1 rw\n"
                ),
                Ok(MappingRule::BtrfsSuperblock),
            ),
            (
                "sibling nested ext4",
                sb1.clone(),
                format!(
                    "{info}{}",
                    mount(
                        Device {
                            major: 259,
                            minor: 3
                        },
                        "/",
                        &format!("{d}/data"),
                        "",
                        "ext4"
                    )
                ),
                Ok(MappingRule::BtrfsSuperblock),
            ),
            // Refused: another filesystem never supplies a device, even a coinciding one.
            (
                "ext4, mismatching device",
                sb1.clone(),
                other_fs(Device { major: 8, minor: 2 }, "ext4"),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "ext4, coinciding device",
                sb1.clone(),
                other_fs(sb, "ext4"),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "tmpfs, coinciding device",
                sb1.clone(),
                other_fs(sb, "tmpfs"),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "overlay, coinciding device",
                sb1.clone(),
                other_fs(sb, "overlay"),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "ext4 nested over btrfs",
                sb1.clone(),
                format!(
                    "{ROOT_EXT4}{}{}",
                    btrfs("/@home", dir.parent()),
                    mount(sb, "/", d, "", "ext4")
                ),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "not a component boundary",
                sb1.clone(),
                format!("{ROOT_EXT4}{}", btrfs("/@home", &d[..d.len() - 1])),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "equal longest mounts",
                sb1.clone(),
                format!("{info}{}", btrfs("/@home", d)),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "equal longest, conflicting devices",
                sb1.clone(),
                format!(
                    "{info}{}",
                    mount(
                        Device {
                            major: 0,
                            minor: 98
                        },
                        "/@home",
                        d,
                        "",
                        "btrfs"
                    )
                ),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "relative root",
                sb1.clone(),
                format!("{ROOT_EXT4}{}", btrfs("@home", d)),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "dotted root",
                sb1.clone(),
                format!("{ROOT_EXT4}{}", btrfs("/@home/../@", d)),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "trailing-slash root",
                sb1.clone(),
                format!("{ROOT_EXT4}{}", btrfs("/@home/", d)),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "escaped root",
                sb1.clone(),
                format!("{ROOT_EXT4}{}", btrfs("/@ho\\040me", d)),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "malformed ancestor root",
                sb1.clone(),
                format!(
                    "1 0 8:1 rel / rw - ext4 /dev/sda1 rw\n{}",
                    btrfs("/@home", d)
                ),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "escaped unrelated point",
                sb1.clone(),
                format!("{info}31 1 0:40 / /mnt/a\\040b rw - tmpfs t rw\n"),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "dotted unrelated point",
                sb1.clone(),
                format!("{info}31 1 0:40 / /mnt/../x rw - tmpfs t rw\n"),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "trailing-slash unrelated point",
                sb1.clone(),
                format!("{info}31 1 0:40 / /mnt/x/ rw - tmpfs t rw\n"),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "relative unrelated point",
                sb1.clone(),
                format!("{info}31 1 0:40 / mnt rw - tmpfs t rw\n"),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "no separator",
                sb1.clone(),
                line(&format!("36 1 0:29 /@home {d} rw btrfs /dev/x rw")),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "separator before the options",
                sb1.clone(),
                line(&format!("36 1 0:29 /@home {d} - btrfs /dev/x rw")),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "an extra field after it",
                sb1.clone(),
                line(&format!("36 1 0:29 /@home {d} rw - btrfs /dev/x rw extra")),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "a missing field after it",
                sb1.clone(),
                line(&format!("36 1 0:29 /@home {d} rw - btrfs /dev/x")),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "a doubled space",
                sb1.clone(),
                line(&format!("36 1 0:29 /@home {d} rw  - btrfs /dev/x rw")),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "a blank line",
                sb1.clone(),
                format!("{info}\n"),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "empty mountinfo",
                sb1.clone(),
                String::new(),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "a nonnumeric mount ID",
                sb1.clone(),
                info.replacen("36 1 ", "x 1 ", 1),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "an overflowing mount ID",
                sb1.clone(),
                info.replacen("36 1 ", "99999999999999999999999 1 ", 1),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "a hexadecimal mount device",
                sb1.clone(),
                line(&format!("36 1 0:1d /@home {d} rw - btrfs /dev/x rw")),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "a mount device without a minor",
                sb1.clone(),
                line(&format!("36 1 0: /@home {d} rw - btrfs /dev/x rw")),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "a mount device without a major",
                sb1.clone(),
                line(&format!("36 1 :29 /@home {d} rw - btrfs /dev/x rw")),
                Err(NOT_THE_ARTIFACT),
            ),
            // Refused mappings.
            (
                "forged inode",
                mapping(exact, id.ino + 1, &client),
                String::new(),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "zero inode",
                raw(&exact_text, "0", &client),
                String::new(),
                Err(malformed),
            ),
            (
                "malformed inode",
                raw(&exact_text, "12a", &client),
                String::new(),
                Err(malformed),
            ),
            (
                "overflowing inode",
                raw(&exact_text, "99999999999999999999999", &client),
                String::new(),
                Err(malformed),
            ),
            (
                "wrong device",
                mapping(
                    Device {
                        major: 0xfe,
                        minor: 0xfe,
                    },
                    id.ino,
                    &client,
                ),
                ROOT_EXT4.into(),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "0x device",
                raw("0x08:01", &ino, &client),
                String::new(),
                Err(malformed),
            ),
            (
                "signed device",
                raw("+8:01", &ino, &client),
                String::new(),
                Err(malformed),
            ),
            (
                "device without a minor",
                raw("08:", &ino, &client),
                String::new(),
                Err(malformed),
            ),
            (
                "overflowing device",
                raw("100000000:01", &ino, &client),
                String::new(),
                Err(malformed),
            ),
            (
                "another inode, same bytes",
                mapping(stat_device(other_id.dev), other_id.ino, &other),
                String::new(),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "a wrong second client",
                format!(
                    "{ex1}\n{}",
                    mapping(stat_device(other_id.dev), other_id.ino, &other)
                ),
                String::new(),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "a renamed versioned client",
                format!(
                    "{ex1}\n{}",
                    mapping(exact, id.ino, &format!("{d}/libfdb_c_7.1.so"))
                ),
                String::new(),
                Err(NOT_THE_ARTIFACT),
            ),
            (
                "deleted",
                format!("{ex1} (deleted)"),
                String::new(),
                Err(ambiguous),
            ),
            (
                "ambiguous pathname",
                mapping(exact, id.ino, &format!("{d}/a b/libfdb_c.so")),
                String::new(),
                Err(ambiguous),
            ),
            (
                "malformed after a passing segment",
                format!("{ex1}\n{}", raw("0x08:01", &ino, &client)),
                String::new(),
                Err(malformed),
            ),
            (
                "mixed rules",
                format!("{ex1}\n{sb1}"),
                info.clone(),
                Err("the native client's mappings were proved by different rules"),
            ),
            (
                "absent",
                LIBC.into(),
                info.clone(),
                Err("the pinned native client is not mapped"),
            ),
        ];
        assert_eq!(cases.len(), 59);
        assert_eq!(cases.iter().filter(|c| c.3.is_ok()).count(), 13);
        for (name, maps, mounts, want) in cases {
            let (got, _) = verdict(&maps, &mounts, id);
            match (want, got) {
                (Ok(rule), Ok(got)) => assert_eq!(got, (rule, 1), "{name}"),
                (Err(message), Err(got)) => {
                    assert_eq!(got, message, "{name}");
                    assert!(!got.contains(d) && !got.contains(".so"), "{name}: {got}");
                }
                (want, got) => panic!("{name}: got {got:?}, want {want:?}"),
            }
        }
        // A job may name a symlink to the artifact: identity, not spelling, is compared.
        let link = format!("{d}/link.so");
        std::os::unix::fs::symlink(&client, &link).unwrap();
        assert_ne!(link, client);
        assert_eq!(
            verdict(&ex1, "", identity(&link)),
            (Ok((MappingRule::ExactStat, 1)), 1)
        );
    }

    #[test]
    fn a_replaced_artifact_path_no_longer_proves_its_old_mapping() {
        let dir = Dir::new("replaced");
        let client = dir.file("libfdb_c.so", b"pinned client");
        let old = identity(&client);
        let sb = superblock(old);
        let info = format!(
            "{ROOT_EXT4}{}",
            mount(sb, "/@home", dir.path(), "", "btrfs")
        );
        let old_exact = mapping(stat_device(old.dev), old.ino, &client);
        let old_btrfs = mapping(sb, old.ino, &client);
        assert_eq!(
            verdict(&old_exact, &info, old).0,
            Ok((MappingRule::ExactStat, 1))
        );
        assert_eq!(
            verdict(&old_btrfs, &info, old).0,
            Ok((MappingRule::BtrfsSuperblock, 1))
        );
        let replacement = dir.file("replacement", b"pinned client");
        fs::rename(&replacement, &client).unwrap();
        let new = identity(&client);
        assert_ne!(new.ino, old.ino, "the replacement kept the inode");
        assert_eq!(fs::read(&client).unwrap(), b"pinned client");
        for (name, maps) in [("exact", &old_exact), ("btrfs", &old_btrfs)] {
            for artifact in [old, new] {
                assert_eq!(
                    verdict(maps, &info, artifact).0,
                    Err(NOT_THE_ARTIFACT.into()),
                    "old {name} mapping"
                );
            }
        }
        let own = mapping(stat_device(new.dev), new.ino, &client);
        assert_eq!(verdict(&own, &info, new).0, Ok((MappingRule::ExactStat, 1)));
    }

    #[test]
    fn unreadable_process_files_fail_closed() {
        let dir = Dir::new("unreadable");
        let client = dir.file("libfdb_c.so", b"pinned client");
        let id = identity(&client);
        let maps = mapping(stat_device(id.dev), id.ino, &client);
        let denied = || std::io::Error::other("denied");
        let err = prove_mapped(|_| Err(denied()), id).unwrap_err();
        assert_eq!(err.message, "cannot read the process mappings");
        // An exact mapping does not excuse unreadable mounts.
        let err = prove_mapped(
            |p| {
                if p == "/proc/self/maps" {
                    Ok(maps.clone())
                } else {
                    Err(denied())
                }
            },
            id,
        )
        .unwrap_err();
        assert_eq!(err.message, "cannot read the process mounts");
        let read = |p: &str| {
            Ok(if p == "/proc/self/maps" {
                maps.clone()
            } else {
                String::new()
            })
        };
        assert_eq!(prove_mapped(read, id).unwrap(), MappingRule::ExactStat);
    }

    #[test]
    fn the_pin_is_checked_before_loading() {
        let dir = Dir::new("pin");
        let wrong = dir.file("libfdb_c.so", b"not the pinned client");
        let message = |path: &str| Api::load(Path::new(path)).err().unwrap().message;
        assert_eq!(
            message(&wrong),
            "libfdb_c SHA256 does not match MODULE.bazel 7.3.77 pin"
        );
        assert_eq!(
            message(&format!("{}/absent.so", dir.path())),
            "cannot read explicit libfdb_c file"
        );
    }

    #[test]
    fn mapping_is_proved_after_loading_and_a_refusal_releases_the_load() {
        let events = std::cell::RefCell::new(Vec::<&str>::new());
        struct Loaded<'a>(&'a std::cell::RefCell<Vec<&'static str>>);
        impl Drop for Loaded<'_> {
            fn drop(&mut self) {
                self.0.borrow_mut().push("drop");
            }
        }
        let load = || {
            events.borrow_mut().push("load");
            Ok(Loaded(&events))
        };
        let refused = load_proved(load, || {
            events.borrow_mut().push("prove");
            Err(mapping_error(NOT_THE_ARTIFACT))
        });
        assert_eq!(refused.err().unwrap().message, NOT_THE_ARTIFACT);
        assert_eq!(*events.borrow(), ["load", "prove", "drop"]);
        events.borrow_mut().clear();
        let failed = load_proved(
            || {
                events.borrow_mut().push("load");
                Err::<Loaded, _>(Error::new(ErrorKind::Config, "load failed"))
            },
            || {
                events.borrow_mut().push("prove");
                Ok(MappingRule::ExactStat)
            },
        );
        assert!(failed.is_err());
        assert_eq!(*events.borrow(), ["load"]);
        events.borrow_mut().clear();
        let (loaded, rule) = load_proved(load, || {
            events.borrow_mut().push("prove");
            Ok(MappingRule::BtrfsSuperblock)
        })
        .ok()
        .unwrap();
        assert_eq!(rule, MappingRule::BtrfsSuperblock);
        assert_eq!(*events.borrow(), ["load", "prove"]);
        drop(loaded);
        assert_eq!(*events.borrow(), ["load", "prove", "drop"]);
        assert_eq!(
            [
                MappedClientProof {
                    rule: MappingRule::ExactStat
                }
                .rule(),
                MappedClientProof {
                    rule: MappingRule::BtrfsSuperblock
                }
                .rule(),
            ],
            ["exact_stat", "btrfs_superblock"]
        );
    }
}
