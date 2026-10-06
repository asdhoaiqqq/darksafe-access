// Output-failure coverage for `darksafe review`. A review is only successful
// when the COMPLETE report landed on stdout without a write error: a receiver
// that refuses the write, or one that accepts only some bytes without even
// reporting an error, must yield exit 1 and a stderr explanation, regardless
// of whether the archive validated and the decision recomputed. These tests
// drive runReview with fault-injecting writers in both text and --json modes.
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// errSimulatedBrokenStdout stands in for a receiver that explicitly refuses
// the write (broken pipe, closed reader, ENOSPC surfaced as an error, ...).
var errSimulatedBrokenStdout = errors.New("simulated broken stdout")

// rejectingWriter accepts nothing: every Write fails with the sentinel and
// reports zero bytes delivered.
type rejectingWriter struct{}

func (rejectingWriter) Write(p []byte) (int, error) {
	return 0, errSimulatedBrokenStdout
}

// partialRejectingWriter accepts up to cap bytes, then refuses the rest with
// the sentinel. The accepted prefix stays observable through the embedded
// buffer.
type partialRejectingWriter struct {
	got bytes.Buffer
	cap int
}

func (w *partialRejectingWriter) Write(p []byte) (int, error) {
	if w.cap <= 0 {
		return 0, errSimulatedBrokenStdout
	}
	n := min(len(p), w.cap)
	w.got.Write(p[:n])
	w.cap -= n
	if n < len(p) {
		return n, errSimulatedBrokenStdout
	}
	return n, nil
}

// shortWriter silently accepts at most cap bytes and never returns an error,
// modeling a receiver that takes only part of the report (a full disk that
// swallowed the error, a consumer that closed early without signaling, etc.).
type shortWriter struct {
	got bytes.Buffer
	cap int
}

func (w *shortWriter) Write(p []byte) (int, error) {
	n := min(len(p), w.cap)
	w.got.Write(p[:n])
	w.cap -= n
	return n, nil
}

// fullReviewPayload runs one successful review into a buffer and returns the
// exact bytes the command tries to deliver, so output-failure tests can
// compare any accepted prefix byte-for-byte.
func fullReviewPayload(t *testing.T, args []string) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := runReview(args, &stdout, &stderr); code != 0 {
		t.Fatalf("control review must succeed: code=%d stderr=%q", code, stderr.String())
	}
	return append([]byte(nil), stdout.Bytes()...)
}

// assertOutputFailure checks the common output-failure contract: exit 1, a
// non-empty stderr that explains the failed report delivery and never blames
// the archive, the target, the historical version or a decision disagreement
// for what was purely an output problem.
func assertOutputFailure(t *testing.T, code int, stderr string) {
	t.Helper()
	if code != 1 {
		t.Fatalf("an incomplete report must exit 1, got %d; stderr=%q", code, stderr)
	}
	if stderr == "" {
		t.Fatal("an output failure must be explained on stderr")
	}
	if !strings.Contains(stderr, "review report") {
		t.Fatalf("stderr must name the review report output, got %q", stderr)
	}
	for _, blame := range []string{
		"archive validation", "version not found", "not a decision",
		"decisions differ",
	} {
		if strings.Contains(stderr, blame) {
			t.Fatalf("a pure output failure must not be described as %q: %q", blame, stderr)
		}
	}
}

// TestReviewTextOutputRefused: the text receiver refuses everything, so
// stdout stays empty and the concrete write error is preserved on stderr.
func TestReviewTextOutputRefused(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	var stderr bytes.Buffer
	code := runReview(reviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint),
		rejectingWriter{}, &stderr)
	assertOutputFailure(t, code, stderr.String())
	if !strings.Contains(stderr.String(), errSimulatedBrokenStdout.Error()) {
		t.Fatalf("the concrete write error must be preserved, got %q", stderr.String())
	}
}

// TestReviewJSONOutputRefused is the --json counterpart: refusal means an
// empty stdout (no partial object) and the preserved write error.
func TestReviewJSONOutputRefused(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	var stderr bytes.Buffer
	code := runReview(jsonReviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint),
		rejectingWriter{}, &stderr)
	assertOutputFailure(t, code, stderr.String())
	if !strings.Contains(stderr.String(), errSimulatedBrokenStdout.Error()) {
		t.Fatalf("the concrete write error must be preserved, got %q", stderr.String())
	}
}

// TestReviewTextOutputShortWriteNoError is the core regression for the text
// mode: a receiver that accepts only part of the report AND returns no error
// must still fail. The accepted prefix stays exactly as written, with no
// appended retries, format switches or success notices.
func TestReviewTextOutputShortWriteNoError(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	args := reviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint)
	full := fullReviewPayload(t, args)

	const limit = 23 // lands mid-report, mid-line
	w := &shortWriter{cap: limit}
	var stderr bytes.Buffer
	code := runReview(args, w, &stderr)
	assertOutputFailure(t, code, stderr.String())

	got := w.got.Bytes()
	if len(got) != limit {
		t.Fatalf("accepted prefix length = %d, want %d", len(got), limit)
	}
	if !bytes.Equal(got, full[:limit]) {
		t.Fatalf("accepted prefix altered:\n got %q\nwant %q", got, full[:limit])
	}
	if !strings.Contains(stderr.String(), "incomplete") {
		t.Fatalf("a byte shortage must be reported as incomplete output, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "wrote 23 of") {
		t.Fatalf("stderr must show the short byte count, got %q", stderr.String())
	}
	// The cut prefix is mid-report and must not be followed by the closing
	// consistency verdict (nothing may be appended after the failure).
	if bytes.Contains(got, []byte("consistent:")) {
		t.Fatalf("nothing may be appended after the short write:\n%q", got)
	}
}

// TestReviewJSONOutputShortWriteNoError is the JSON counterpart: a silent
// short write leaves a truncated object (not valid JSON), exits 1 and reports
// the shortage without retrying in text form.
func TestReviewJSONOutputShortWriteNoError(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	args := jsonReviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint)
	full := fullReviewPayload(t, args)

	const limit = 12 // inside the object, far from the closing brace
	w := &shortWriter{cap: limit}
	var stderr bytes.Buffer
	code := runReview(args, w, &stderr)
	assertOutputFailure(t, code, stderr.String())

	got := w.got.Bytes()
	if len(got) != limit || !bytes.Equal(got, full[:limit]) {
		t.Fatalf("accepted prefix mismatch:\n got %q\nwant %q", got, full[:limit])
	}
	if bytes.Contains(got, []byte("target sequence:")) {
		t.Fatal("a JSON failure must not be retried in text format")
	}
	if bytes.Contains(got, []byte("}")) {
		t.Fatalf("a truncated JSON object must remain truncated, got %q", got)
	}
	if !strings.Contains(stderr.String(), "incomplete") ||
		!strings.Contains(stderr.String(), "wrote 12 of") {
		t.Fatalf("stderr must report the byte shortage, got %q", stderr.String())
	}
}

// TestReviewTextOutputRefusedAfterPrefix covers an explicit error that
// arrives only after the receiver took a prefix: the error reason wins, the
// prefix is left in place, and the command still exits 1.
func TestReviewTextOutputRefusedAfterPrefix(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	args := reviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint)
	full := fullReviewPayload(t, args)

	const limit = 40
	w := &partialRejectingWriter{cap: limit}
	var stderr bytes.Buffer
	code := runReview(args, w, &stderr)
	assertOutputFailure(t, code, stderr.String())
	if !strings.Contains(stderr.String(), errSimulatedBrokenStdout.Error()) {
		t.Fatalf("the concrete write error must be preserved, got %q", stderr.String())
	}
	if got := w.got.Bytes(); !bytes.Equal(got, full[:limit]) {
		t.Fatalf("accepted prefix altered:\n got %q\nwant %q", got, full[:limit])
	}
}

// TestReviewJSONOutputRefusedAfterPrefix is the JSON counterpart.
func TestReviewJSONOutputRefusedAfterPrefix(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	args := jsonReviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint)
	full := fullReviewPayload(t, args)

	const limit = 5
	w := &partialRejectingWriter{cap: limit}
	var stderr bytes.Buffer
	code := runReview(args, w, &stderr)
	assertOutputFailure(t, code, stderr.String())
	if !strings.Contains(stderr.String(), errSimulatedBrokenStdout.Error()) {
		t.Fatalf("the concrete write error must be preserved, got %q", stderr.String())
	}
	if got := w.got.Bytes(); !bytes.Equal(got, full[:limit]) {
		t.Fatalf("accepted prefix altered:\n got %q\nwant %q", got, full[:limit])
	}
}

// TestReviewOutputShortZeroBytesNoError: a receiver that takes nothing and
// returns no error at all leaves stdout empty in both modes and is still an
// incomplete-output failure (never a success with an empty "report").
func TestReviewOutputShortZeroBytesNoError(t *testing.T) {
	path, cp := fixtureChain(t, "acme")
	for name, args := range map[string][]string{
		"text": reviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint),
		"json": jsonReviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint),
	} {
		t.Run(name, func(t *testing.T) {
			w := &shortWriter{cap: 0}
			var stderr bytes.Buffer
			code := runReview(args, w, &stderr)
			assertOutputFailure(t, code, stderr.String())
			if w.got.Len() != 0 {
				t.Fatalf("stdout must be empty, got %q", w.got.String())
			}
			if !strings.Contains(stderr.String(), "wrote 0 of") {
				t.Fatalf("zero accepted bytes must be reported, got %q", stderr.String())
			}
		})
	}
}

// TestReviewInconsistentMaterialStillFailsOnOutputFailure proves the output
// check is independent of the business verdict: even legitimate material
// whose two decisions disagree does not turn a truncated review into success.
func TestReviewInconsistentMaterialStillFailsOnOutputFailure(t *testing.T) {
	path, cp := writeInconsistentArchive(t)
	cases := map[string][]string{
		"text": reviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint),
		"json": jsonReviewArgs(path, "acme", 2, cp.EndSeq, cp.Fingerprint),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			w := &shortWriter{cap: 8}
			var stderr bytes.Buffer
			code := runReview(args, w, &stderr)
			assertOutputFailure(t, code, stderr.String())
			if w.got.Len() != 8 {
				t.Fatalf("only the accepted prefix may remain, got %q", w.got.String())
			}
		})
	}
}
