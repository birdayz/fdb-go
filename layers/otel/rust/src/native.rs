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
    ffi::{CString, c_char, c_void},
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
    select: unsafe extern "C" fn(i32, i32) -> Code,
    setup: unsafe extern "C" fn() -> Code,
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
            checked((api.select)(730, 730))?;
            checked((api.setup)())?;
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
    let mut canceled = false;
    loop {
        // Pending plain ThreadSingleAssignmentVar::cancel queues to the network
        // thread (ThreadHelper.actor.h); cancellation cannot drain a dead loop.
        network_check(exit)?;
        if ctx.check().is_err() && !canceled {
            cancel();
            canceled = true;
        }
        match receiver.recv_timeout(Duration::from_millis(10)) {
            Ok(()) => return if canceled { ctx.check() } else { Ok(()) },
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
            let (mut out, mut more) = self.collect_range(future, ctx)?;
            let mut total = out.iter().map(|(k, v)| k.len() + v.len()).sum::<usize>();
            while more {
                let Some((last, _)) = out.last() else {
                    return Err(Error::new(
                        ErrorKind::Operation,
                        "empty nonterminal FDB range",
                    ));
                };
                let mut next = last.clone();
                next.push(0);
                let (page, has_more) = self.collect_range(self.range_future(&next, end), ctx)?;
                total += page.iter().map(|(k, v)| k.len() + v.len()).sum::<usize>();
                if total > 16 * 1024 * 1024 || out.len() + page.len() > 100_000 {
                    return Err(Error::new(
                        ErrorKind::Operation,
                        "stored prior image exceeds read bound",
                    ));
                }
                out.extend(page);
                more = has_more;
            }
            result.push(out);
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
    #[test]
    fn dead_network_does_not_wait_for_an_unreachable_callback() {
        let (_sender, receiver) = mpsc::sync_channel(1);
        let exit = AtomicI32::new(4000);
        let started = std::time::Instant::now();
        assert_eq!(
            wait_ready(&receiver, &Context::default(), &exit, || panic!(
                "dead loop cannot cancel"
            ))
            .unwrap_err()
            .kind,
            ErrorKind::Operation
        );
        assert!(started.elapsed() < Duration::from_secs(1));
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
    fn cancellation_is_issued_once_and_still_joins_readiness() {
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
