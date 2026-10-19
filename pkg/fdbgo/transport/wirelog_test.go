package transport

import (
	"bytes"
	"encoding/binary"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/onsi/gomega"
)

func TestWireLogBinary(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	var buf bytes.Buffer
	wl := &WireLog{w: &buf, binary: true}

	token := UID{First: 0x1234, Second: 0x5678}
	body := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	wl.log('S', token, body)

	data := buf.Bytes()
	g.Expect(len(data)).To(gomega.BeNumerically(">=", 29+4)) // header + body

	// Direction byte
	g.Expect(data[0]).To(gomega.Equal(byte('S')))

	// Token
	first := binary.LittleEndian.Uint64(data[9:17])
	second := binary.LittleEndian.Uint64(data[17:25])
	g.Expect(first).To(gomega.Equal(uint64(0x1234)))
	g.Expect(second).To(gomega.Equal(uint64(0x5678)))

	// Body length
	bodyLen := binary.LittleEndian.Uint32(data[25:29])
	g.Expect(bodyLen).To(gomega.Equal(uint32(4)))

	// Body content
	g.Expect(data[29:33]).To(gomega.Equal([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
}

func TestWireLogText(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	var buf bytes.Buffer
	wl := &WireLog{w: &buf, binary: false}

	token := UID{First: 0xABCD, Second: 0xEF01}
	body := []byte{0x01, 0x02, 0x03}
	wl.log('S', token, body)

	output := buf.String()
	g.Expect(output).To(gomega.ContainSubstring("SEND"))
	g.Expect(output).To(gomega.ContainSubstring("len=3"))
}

func TestWireLogTextRecv(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	var buf bytes.Buffer
	wl := &WireLog{w: &buf, binary: false}

	token := UID{First: 1, Second: 2}
	body := []byte{0xFF}
	wl.log('R', token, body)

	output := buf.String()
	g.Expect(output).To(gomega.ContainSubstring("RECV"))
}

func TestWireLogTextTruncation(t *testing.T) {
	t.Parallel()
	g := gomega.NewWithT(t)

	var buf bytes.Buffer
	wl := &WireLog{w: &buf, binary: false}

	token := UID{First: 0, Second: 0}
	// Body larger than 64 bytes — should be truncated in text output
	body := bytes.Repeat([]byte{0xAA}, 100)
	wl.log('S', token, body)

	output := buf.String()
	g.Expect(output).To(gomega.ContainSubstring("more bytes"))
	g.Expect(output).To(gomega.ContainSubstring("len=100"))
	// Should contain exactly 64 bytes of 0xAA hex (128 hex chars)
	g.Expect(output).To(gomega.ContainSubstring(strings.Repeat("aa", 64)))
	// But NOT 65 bytes worth (would mean truncation didn't work)
	g.Expect(output).NotTo(gomega.ContainSubstring(strings.Repeat("aa", 65)))
}

// A logger loaded before stop writes nothing after stop returns.
func TestCaptureWireLogRetainedAfterStop(t *testing.T) {
	var buf bytes.Buffer
	stop := CaptureWireLog(&buf)
	wl := globalWireLog.Load() // as a producer that loaded it before stop
	wl.log('S', UID{First: 1}, []byte("before"))
	if buf.Len() == 0 {
		t.Fatal("a write before stop was not captured")
	}
	n := buf.Len()
	stop()
	wl.log('S', UID{First: 1}, []byte("after"))
	if buf.Len() != n {
		t.Fatalf("WIRELOG_AFTER_STOP: %d bytes written by a retained logger after stop", buf.Len()-n)
	}
	if globalWireLog.Load() != nil {
		t.Fatal("stop left the capture installed")
	}
}

// gatedWriter blocks its first write until released.
type gatedWriter struct {
	entered, release chan struct{}
	finished         atomic.Bool
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	if w.entered != nil {
		close(w.entered)
		w.entered = nil
		<-w.release
		w.finished.Store(true)
	}
	return len(p), nil
}

// stop waits for a write in progress to finish.
func TestCaptureWireLogStopWaitsForWrite(t *testing.T) {
	w := &gatedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	entered := w.entered
	stop := CaptureWireLog(w)
	go LogSend(UID{First: 1}, []byte("body"))
	<-entered
	stopped := make(chan bool)
	go func() { stop(); stopped <- w.finished.Load() }()
	close(w.release)
	if !<-stopped {
		t.Fatal("WIRELOG_STOP_WAIT: stop returned while a write was in progress")
	}
}
