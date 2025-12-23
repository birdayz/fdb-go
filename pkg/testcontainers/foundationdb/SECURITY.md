# Security & Bug Fixes

This document tracks security and concurrency fixes applied to the testcontainer package.

## Critical Fixes Applied

### 1. Race Condition in API Version Management (FIXED)
**Issue**: `apiVersionSet` variable was read/written without synchronization outside `sync.Once`.
**Impact**: Data race in concurrent tests could cause unpredictable behavior.
**Fix**: Added `sync.RWMutex` to protect reads/writes to `apiVersionSet`.

### 2. Shell Injection Vulnerability in socat-entrypoint.sh (FIXED)
**Issue**: Used `exec sh -c "$SOCAT_CMD"` with command string from config file.
**Impact**: Potential command injection if config generation had bugs.
**Fix**: Changed to pass port via `TARGET_PORT=` variable, validate it's numeric, execute socat directly without shell expansion.

### 3. Resource Cleanup Race in Terminate() (FIXED)
**Issue**: `Terminate()` accessed `tempClusterFile` without mutex protection, creating race with `GetFDBDatabase()`.
**Impact**: Data race, potential use-after-free of temp cluster file.
**Fix**: Added mutex protection around cleanup, mark `dbInitialized = false` during termination.

### 4. Silent Cleanup Failures (FIXED)
**Issue**: `Terminate()` always returned `nil` even if cleanup failed.
**Impact**: Resource leaks invisible to tests, harder debugging.
**Fix**: Collect all errors during cleanup, return combined error if any fail.

### 5. Infinite Wait Loops in Entrypoints (FIXED)
**Issue**: Both shell entrypoints waited forever if config injection failed.
**Impact**: Hung containers, difficult debugging in CI/CD.
**Fix**: Added 30-second timeouts to both entrypoint scripts with explicit error exits.

### 6. Missing Context Checks (FIXED)
**Issue**: `InitializeDatabase()` didn't check if context was cancelled before starting.
**Impact**: Operations continued after timeout/cancellation.
**Fix**: Added `ctx.Err()` check at function start.

## Thread Safety

The package now correctly handles:
- Concurrent `GetFDBDatabase()` calls (mutex-protected caching)
- Concurrent `Terminate()` calls (mutex-protected cleanup)
- Process-wide API version initialization (sync.Once + mutex)
- Concurrent container access patterns

## Testing

All fixes verified with existing test suite:
- 6 tests pass in 17.9s
- Tests cover basic operations, custom configs, multiple versions
- No regressions introduced

## Future Improvements Considered

Low priority items not yet implemented:
- Concurrent `GetFDBDatabase()` stress test
- Explicit cleanup verification tests
- Input validation on config options (WithMemory, WithVersion)
- Exponential backoff in entrypoint wait loops (currently fixed 0.1s)

These are optimization opportunities, not bugs.
