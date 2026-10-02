package darksafe

import (
	"testing"
)

// FuzzDecodeAuditArchive feeds arbitrary bytes to the decoder: it must
// never panic, never return records without a nil error, and anything it
// accepts successfully must itself re-encode back to identical bytes and
// must validate through VerifyAudit against the retained checkpoint.
func FuzzDecodeAuditArchive(f *testing.F) {
	s := NewStore()
	s.Publish("acme", 0, []Policy{allowPolicy("p1", "u1", "read", "org/a", false)})
	s.Decide("acme", request("acme", "u1", "r1", "org/a", "read"))
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		f.Fatal(err)
	}
	seed, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add(seed[:len(seed)-1])
	f.Add(append([]byte(nil), seed...))
	f.Add([]byte(archiveMagic))
	f.Add([]byte{})
	f.Add([]byte(nil))

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := DecodeAuditArchive(data, "acme", cp)
		if err != nil {
			if got != nil {
				t.Fatalf("error with non-nil records: %v", err)
			}
			return
		}
		if got == nil {
			t.Fatal("successful decode returned nil records")
		}
		if err := VerifyAudit("acme", got, cp); err != nil {
			t.Fatalf("accepted material fails VerifyAudit: %v", err)
		}
		reencoded, err := EncodeAuditArchive("acme", got, cp)
		if err != nil {
			t.Fatalf("accepted material does not re-encode: %v", err)
		}
		if string(reencoded) != string(data) {
			t.Fatalf("round trip changed the archive bytes")
		}
	})
}
