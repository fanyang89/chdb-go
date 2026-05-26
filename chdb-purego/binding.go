package chdbpurego

import (
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// sigactionBufSize is large enough to hold a struct sigaction on every
// platform we care about: 16 B on macOS, ~152 B on glibc/Linux.
const sigactionBufSize = 256

// signalsToProtect lists the signals the Go runtime owns and must keep
// owning. libchdb's chdb_set_signal_handlers_enabled(0) wipes the kernel's
// handler list for the same set as a side effect; we save and restore these
// around that call so Go's handlers survive.
//
// Signal numbers are NOT the same on Linux and macOS (e.g. SIGBUS is 10 on
// Darwin but 7 on Linux; SIGURG is 16 on Darwin but 23 on Linux). Use the
// syscall package's per-platform constants so the values resolve correctly
// at compile time on each OS.
var signalsToProtect = []int{
	int(syscall.SIGILL),
	int(syscall.SIGABRT),
	int(syscall.SIGFPE),
	int(syscall.SIGBUS),
	int(syscall.SIGSEGV),
	int(syscall.SIGURG), // Go uses SIGURG for async preemption
}

// libcSigaction is the libc sigaction(2) function, resolved from whichever
// libc the process was already linked against. We pass opaque buffers
// rather than typed structs so the same code works for the differently-
// laid-out struct sigaction on macOS vs glibc.
var libcSigaction func(sig int, act, oact unsafe.Pointer) int

func loadSigaction() {
	// Try the libc that this process was already linked against.
	// Order matters: try platform-canonical paths first.
	candidates := []string{
		"/usr/lib/libSystem.B.dylib", // macOS
		"libc.so.6",                  // glibc (ldconfig will resolve)
		"/lib/x86_64-linux-gnu/libc.so.6",
		"/lib/aarch64-linux-gnu/libc.so.6",
		"libc.so", // musl + general fallback
	}
	var libc uintptr
	var lastErr error
	for _, p := range candidates {
		if lib, err := purego.Dlopen(p, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
			libc = lib
			lastErr = nil
			break
		} else {
			lastErr = err
		}
	}
	if libc == 0 {
		panic("chdb-purego: cannot dlopen libc to resolve sigaction(2): " + lastErr.Error())
	}
	purego.RegisterLibFunc(&libcSigaction, libc, "sigaction")
}

// snapshotSignalHandlers stores the current sigaction state for the signals
// we need to protect, in opaque buffers.
func snapshotSignalHandlers() [][sigactionBufSize]byte {
	saved := make([][sigactionBufSize]byte, len(signalsToProtect))
	for i, sig := range signalsToProtect {
		libcSigaction(sig, nil, unsafe.Pointer(&saved[i][0]))
	}
	return saved
}

// restoreSignalHandlers writes a previously-snapshotted sigaction state back.
func restoreSignalHandlers(saved [][sigactionBufSize]byte) {
	for i, sig := range signalsToProtect {
		libcSigaction(sig, unsafe.Pointer(&saved[i][0]), nil)
	}
}

func findLibrary() string {
	// Env var
	if envPath := os.Getenv("CHDB_LIB_PATH"); envPath != "" {
		return envPath
	}

	// ldconfig with Linux
	if path, err := exec.LookPath("libchdb.so"); err == nil {
		return path
	}

	// default path
	commonPaths := []string{
		"/usr/local/lib/libchdb.so",
		"/opt/homebrew/lib/libchdb.dylib",
	}

	for _, p := range commonPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	//should be an error ?
	return "libchdb.so"
}

var (
	// old API
	queryStable            func(argc int, argv []string) *local_result
	freeResult             func(result *local_result)
	queryStableV2          func(argc int, argv []string) *local_result_v2
	freeResultV2           func(result *local_result_v2)
	connectChdb            func(argc int, argv []*byte) **chdb_conn
	closeConn              func(conn **chdb_conn)
	queryConn              func(conn *chdb_conn, query string, format string) *local_result_v2
	queryConnStreaming     func(conn *chdb_conn, query string, format string) *chdb_streaming_result
	streamingResultError   func(result *chdb_streaming_result) *string
	streamingResultNext    func(conn *chdb_conn, result *chdb_streaming_result) *local_result_v2
	streamingResultDestroy func(result *chdb_streaming_result)
	streamingResultCancel  func(conn *chdb_conn, result *chdb_streaming_result)

	// new API
	chdbConnect                func(argc int, argv []*byte) *chdb_connection
	chdbCloseConn              func(conn *chdb_connection)
	chdbQuery                  func(conn unsafe.Pointer, query string, format string) *chdb_result
	chdbStreamQuery            func(conn unsafe.Pointer, query string, format string) *chdb_result
	chdbStreamFetchResult      func(conn unsafe.Pointer, result *chdb_result) *chdb_result
	chdbStreamCancelQuery      func(conn *chdb_connection, result *chdb_result)
	chdbDestroyQueryResult     func(result *chdb_result)
	chdbResultBuffer           func(result *chdb_result) *byte
	chdbResultLen              func(result *chdb_result) uint    //size_t
	chdbResultElapsed          func(result *chdb_result) float64 // double
	chdbResultRowsRead         func(result *chdb_result) uint64
	chdbResultBytesRead        func(result *chdb_result) uint64
	chdbResultStorageRowsRead  func(result *chdb_result) uint64
	chdbResultStorageBytesRead func(result *chdb_result) uint64
	chdbResultError            func(result *chdb_result) string

	// Process-wide signal handler control. See issue #30: by default libchdb
	// installs its own SIGSEGV/SIGABRT/SIGBUS/SIGILL handlers when a
	// connection is opened, which overwrites the Go runtime's handlers and
	// breaks async preemption (SIGURG) and stack-growth (SIGSEGV) handling.
	chdbSetSignalHandlersEnabled func(enabled int)
	chdbResetSignalHandlers      func()
)

func init() {
	path := findLibrary()
	libchdb, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		panic(err)
	}
	purego.RegisterLibFunc(&queryStable, libchdb, "query_stable")
	purego.RegisterLibFunc(&freeResult, libchdb, "free_result")
	purego.RegisterLibFunc(&queryStableV2, libchdb, "query_stable_v2")

	purego.RegisterLibFunc(&freeResultV2, libchdb, "free_result_v2")
	purego.RegisterLibFunc(&connectChdb, libchdb, "connect_chdb")
	purego.RegisterLibFunc(&closeConn, libchdb, "close_conn")
	purego.RegisterLibFunc(&queryConn, libchdb, "query_conn")
	purego.RegisterLibFunc(&queryConnStreaming, libchdb, "query_conn_streaming")
	purego.RegisterLibFunc(&streamingResultError, libchdb, "chdb_streaming_result_error")
	purego.RegisterLibFunc(&streamingResultNext, libchdb, "chdb_streaming_fetch_result")
	purego.RegisterLibFunc(&streamingResultCancel, libchdb, "chdb_streaming_cancel_query")
	purego.RegisterLibFunc(&streamingResultDestroy, libchdb, "chdb_destroy_result")

	// new API
	purego.RegisterLibFunc(&chdbConnect, libchdb, "chdb_connect")
	purego.RegisterLibFunc(&chdbCloseConn, libchdb, "chdb_close_conn")
	purego.RegisterLibFunc(&chdbQuery, libchdb, "chdb_query")
	purego.RegisterLibFunc(&chdbStreamQuery, libchdb, "chdb_stream_query")
	purego.RegisterLibFunc(&chdbStreamFetchResult, libchdb, "chdb_stream_fetch_result")
	purego.RegisterLibFunc(&chdbStreamCancelQuery, libchdb, "chdb_stream_cancel_query")
	purego.RegisterLibFunc(&chdbDestroyQueryResult, libchdb, "chdb_destroy_query_result")
	purego.RegisterLibFunc(&chdbResultBuffer, libchdb, "chdb_result_buffer")
	purego.RegisterLibFunc(&chdbResultLen, libchdb, "chdb_result_length")
	purego.RegisterLibFunc(&chdbResultElapsed, libchdb, "chdb_result_elapsed")
	purego.RegisterLibFunc(&chdbResultRowsRead, libchdb, "chdb_result_rows_read")
	purego.RegisterLibFunc(&chdbResultBytesRead, libchdb, "chdb_result_bytes_read")
	purego.RegisterLibFunc(&chdbResultStorageRowsRead, libchdb, "chdb_result_storage_rows_read")
	purego.RegisterLibFunc(&chdbResultStorageBytesRead, libchdb, "chdb_result_storage_bytes_read")
	purego.RegisterLibFunc(&chdbResultError, libchdb, "chdb_result_error")

	purego.RegisterLibFunc(&chdbSetSignalHandlersEnabled, libchdb, "chdb_set_signal_handlers_enabled")
	purego.RegisterLibFunc(&chdbResetSignalHandlers, libchdb, "chdb_reset_signal_handlers")

	// Tell libchdb NOT to install its own signal handlers on the first
	// chdb_connect(). Without this, libchdb's ClickHouse daemon code
	// installs handlers for SIGSEGV / SIGABRT / SIGBUS / SIGILL / SIGFPE
	// the first time a connection is opened, overwriting the handlers
	// the Go runtime relies on for stack growth and panic recovery. When
	// a Go signal then lands in libchdb's handler instead of the
	// runtime's, the result is the rare std::mutex::unlock crash
	// described in issue #30 — most often on macOS arm64 CI where signal
	// pressure is highest.
	//
	// Important: chdb_set_signal_handlers_enabled(0) doesn't only set a
	// flag — it also wipes the existing handlers in the same set back to
	// SIG_DFL (verified empirically; the chdb.h header doesn't mention
	// this side effect). Since the Go runtime has already installed its
	// handlers by the time this package's init runs, we have to save Go's
	// handlers, call the disable, then restore them. After this dance the
	// libchdb flag is set (so no future connect installs handlers) and
	// Go's handlers remain in place.
	loadSigaction()
	saved := snapshotSignalHandlers()
	chdbSetSignalHandlersEnabled(0)
	restoreSignalHandlers(saved)
}
