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
const CLIENT_SHA256: &str = "617572467897d0360f76ffc33f24b58090ee691e3273750d2ff334cc71aae3fa";
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
    fn load(path: &Path) -> Result<Self> {
        let wire = fs::read(path)
            .map_err(|_| Error::new(ErrorKind::Config, "cannot read explicit libfdb_c file"))?;
        if hex::encode(Sha256::digest(&wire)) != CLIENT_SHA256 {
            return Err(Error::new(
                ErrorKind::Config,
                "libfdb_c SHA256 does not match MODULE.bazel 7.3.77 pin",
            ));
        }
        // Only an explicitly named, hash-verified local artifact is executable.
        let library = unsafe { Library::new(path) }.map_err(|_| {
            Error::new(
                ErrorKind::Config,
                "cannot load pinned libfdb_c or its system dependencies",
            )
        })?;
        macro_rules! symbol {
            ($name:literal) => {
                unsafe {
                    *library.get(concat!($name, "\0").as_bytes()).map_err(|_| {
                        Error::new(ErrorKind::Config, concat!("missing C API symbol ", $name))
                    })?
                }
            };
        }
        Ok(Self {
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
        })
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
}
// libfdb_c databases are documented thread-safe; all transaction handles remain
// thread-local and futures copy their borrowed C data before destruction.
unsafe impl Send for NativeDatabase {}
unsafe impl Sync for NativeDatabase {}

impl NativeDatabase {
    /// Opening starts the native client's one-shot network thread, but never
    /// starts an FDB server. Call only against an explicitly admitted cluster.
    pub fn open(client: &Path, cluster_file: &Path) -> Result<Self> {
        refuse_network_option_env(std::env::vars_os())?;
        if !client.is_absolute() || !cluster_file.is_absolute() {
            return Err(Error::new(
                ErrorKind::Config,
                "absolute explicit client and cluster paths required",
            ));
        }
        let cluster = CString::new(cluster_file.as_os_str().as_encoded_bytes())
            .map_err(|_| Error::new(ErrorKind::Config, "invalid cluster path"))?;
        let api = Arc::new(Api::load(client)?);
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
        })
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
}
