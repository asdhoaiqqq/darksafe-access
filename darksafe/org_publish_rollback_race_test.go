package darksafe

import (
	"errors"
	"reflect"
	"testing"
)

// These tests pin down how Publish and Rollback behave when they race on
// the same expected version: exactly one of them may win, the loser gets a
// distinguishable version conflict, and the audit chain, the version
// counter and every later decision reflect only the winner. Neither test
// assumes a fixed winner; both outcomes are checked against the same rules.

// raceOutcome captures one side of a publish-versus-rollback race.
type raceOutcome struct {
	version int
	err     error
}

// raceFixture is an organization with two published versions of different
// content, ready for a publish-versus-rollback race on the current version.
//
// The three policy sets are chosen so the two candidate new versions
// decide the same request differently and carry distinguishable policy
// IDs: the rollback target (v1Set) allows u1, the published set denies u1.
type raceFixture struct {
	store            *Store
	v1Set, v2Set     []Policy
	pubSet           []Policy
	before           []AuditRecord // full export taken before the race
	beforeCheckpoint Checkpoint
}

// newRaceFixture publishes v1Set as version 1 and v2Set as version 2 and
// exports the two-record audit chain as the pre-race baseline.
func newRaceFixture(t *testing.T) *raceFixture {
	t.Helper()
	s := NewStore()
	f := &raceFixture{
		store:  s,
		v1Set:  []Policy{allowPolicy("v1-allow", "u1", "read", "org/a", false)},
		v2Set:  []Policy{allowPolicy("v2-allow", "u2", "read", "org/a", false)},
		pubSet: []Policy{{ID: "pub-deny", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny}},
	}
	v1, err := s.Publish("acme", 0, f.v1Set)
	if err != nil || v1 != 1 {
		t.Fatalf("setup publish v1 = %d, %v; want 1, nil", v1, err)
	}
	v2, err := s.Publish("acme", v1, f.v2Set)
	if err != nil || v2 != 2 {
		t.Fatalf("setup publish v2 = %d, %v; want 2, nil", v2, err)
	}
	f.before, f.beforeCheckpoint, err = s.AuditExport("acme", 0)
	if err != nil {
		t.Fatalf("export before race: %v", err)
	}
	if len(f.before) != 2 {
		t.Fatalf("records before race = %d, want 2", len(f.before))
	}
	if err := VerifyAudit("acme", f.before, f.beforeCheckpoint); err != nil {
		t.Fatalf("pre-race chain must verify: %v", err)
	}
	return f
}

// race runs a publish of the third set against a rollback to version 1,
// both declaring the same expected current version (2), and returns both
// outcomes.
func (f *raceFixture) race() (pub, rb raceOutcome) {
	start := make(chan struct{})
	pubCh, rbCh := make(chan raceOutcome, 1), make(chan raceOutcome, 1)
	go func() {
		<-start
		v, err := f.store.Publish("acme", 2, f.pubSet)
		pubCh <- raceOutcome{v, err}
	}()
	go func() {
		<-start
		v, err := f.store.Rollback("acme", 2, 1)
		rbCh <- raceOutcome{v, err}
	}()
	close(start)
	return <-pubCh, <-rbCh
}

// checkRaceWinner verifies the race produced exactly one winner on version
// 3 and reports which operation won and the policy set the new version
// must hold.
func (f *raceFixture) checkRaceWinner(t *testing.T, pub, rb raceOutcome) (publishWon bool, wantSet []Policy) {
	t.Helper()
	publishWon = pub.err == nil
	rollbackWon := rb.err == nil
	if publishWon == rollbackWon {
		t.Fatalf("publish err=%v rollback err=%v: exactly one must succeed", pub.err, rb.err)
	}
	winner, loser := pub, rb
	wantSet = f.pubSet
	if rollbackWon {
		winner, loser = rb, pub
		wantSet = f.v1Set
	}
	if !errors.Is(loser.err, ErrVersionConflict) {
		t.Fatalf("loser err = %v, want ErrVersionConflict", loser.err)
	}
	if loser.version != 0 {
		t.Fatalf("loser version = %d, want 0 (a failed request returns no version)", loser.version)
	}
	// The winner gets the next version: 3, never the rollback target 1.
	if winner.version != 3 {
		t.Fatalf("winner version = %d, want 3", winner.version)
	}
	// The version counter advances exactly once; the loser consumes nothing.
	if got := f.store.CurrentVersion("acme"); got != 3 {
		t.Fatalf("current version = %d, want 3", got)
	}
	if _, err := f.store.Policies("acme", 4); !errors.Is(err, ErrVersionNotFound) {
		t.Fatalf("version 4 must not exist, err = %v", err)
	}
	return publishWon, wantSet
}

// checkVersionContents verifies the new version holds exactly the winner's
// full set — no merging of the two candidate sets, no partial loss, no
// loser content — and that both pre-race versions are intact.
func (f *raceFixture) checkVersionContents(t *testing.T, wantSet []Policy) {
	t.Helper()
	got, err := f.store.Policies("acme", 3)
	if err != nil {
		t.Fatalf("query version 3: %v", err)
	}
	if !reflect.DeepEqual(got, wantSet) {
		t.Fatalf("version 3 policies = %+v, want exactly %+v", got, wantSet)
	}
	for version, want := range map[int][]Policy{1: f.v1Set, 2: f.v2Set} {
		got, err := f.store.Policies("acme", version)
		if err != nil {
			t.Fatalf("query version %d: %v", version, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("version %d changed by the race: got %+v, want %+v", version, got, want)
		}
	}
}

// checkRaceAudit verifies the chain grew by exactly one policy-change
// record that matches the winning operation, with no gaps and no record
// from the loser, and that the full export still verifies.
func (f *raceFixture) checkRaceAudit(t *testing.T, publishWon bool, wantSet []Policy) {
	t.Helper()
	after, cp, err := f.store.AuditExport("acme", 0)
	if err != nil {
		t.Fatalf("export after race: %v", err)
	}
	if len(after) != len(f.before)+1 {
		t.Fatalf("audit records = %d, want %d (exactly one new record)", len(after), len(f.before)+1)
	}
	// The pre-race records are untouched, fingerprints included.
	for i := range f.before {
		if !reflect.DeepEqual(after[i], f.before[i]) {
			t.Fatalf("record %d changed: got %+v, want %+v", i+1, after[i], f.before[i])
		}
	}
	rec := after[len(after)-1]
	if rec.Seq != len(after) {
		t.Fatalf("new record seq = %d, want %d (gapless)", rec.Seq, len(after))
	}
	if rec.Kind != AuditPolicyChange || rec.Change == nil {
		t.Fatalf("new record kind = %q, want %q with a change payload", rec.Kind, AuditPolicyChange)
	}
	ch := rec.Change
	if ch.Version != 3 {
		t.Fatalf("recorded version = %d, want 3", ch.Version)
	}
	if !reflect.DeepEqual(ch.Policies, wantSet) {
		t.Fatalf("recorded policies = %+v, want the winner's set %+v", ch.Policies, wantSet)
	}
	if publishWon {
		// An ordinary publish keeps its ordinary meaning.
		if ch.RolledBack || ch.SourceVersion != 0 {
			t.Fatalf("publish record: RolledBack=%v SourceVersion=%d, want false/0", ch.RolledBack, ch.SourceVersion)
		}
	} else {
		// A rollback record names the historical version it copied.
		if !ch.RolledBack || ch.SourceVersion != 1 {
			t.Fatalf("rollback record: RolledBack=%v SourceVersion=%d, want true/1", ch.RolledBack, ch.SourceVersion)
		}
	}
	if err := VerifyAudit("acme", after, cp); err != nil {
		t.Fatalf("exported chain must verify: %v", err)
	}
}

// checkRaceDecision verifies a subsequent access request is decided by the
// winning version and that the decision record stores the same outcome.
func (f *raceFixture) checkRaceDecision(t *testing.T, publishWon bool) {
	t.Helper()
	req := request("acme", "u1", "r1", "org/a", "read")
	d := f.store.Decide("acme", req)
	want := Decision{Version: 3}
	if publishWon {
		want.Allowed, want.Reason, want.Matched = false, "matched deny policy", []string{"pub-deny"}
	} else {
		want.Allowed, want.Reason, want.Matched = true, "matched allow policy", []string{"v1-allow"}
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("decision = %+v, want %+v (the winning version's outcome)", d, want)
	}

	records, cp, err := f.store.AuditExport("acme", 0)
	if err != nil {
		t.Fatalf("export after decide: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("audit records = %d, want 4 (3 changes + 1 decision)", len(records))
	}
	rec := records[3]
	if rec.Kind != AuditDecision || rec.Decision == nil {
		t.Fatalf("record 4 kind = %q, want %q with a decision payload", rec.Kind, AuditDecision)
	}
	if !reflect.DeepEqual(rec.Decision.Request, req) {
		t.Fatalf("recorded request = %+v, want %+v", rec.Decision.Request, req)
	}
	if !reflect.DeepEqual(rec.Decision.Decision, d) {
		t.Fatalf("recorded decision = %+v, want the returned %+v", rec.Decision.Decision, d)
	}
	if err := VerifyAudit("acme", records, cp); err != nil {
		t.Fatalf("chain with decision record must verify: %v", err)
	}
}

func TestConcurrentPublishRollbackSameVersion(t *testing.T) {
	const rounds = 32
	wins := map[bool]int{} // publishWon -> count
	for round := 0; round < rounds; round++ {
		f := newRaceFixture(t)
		pub, rb := f.race()

		publishWon, wantSet := f.checkRaceWinner(t, pub, rb)
		wins[publishWon]++
		f.checkVersionContents(t, wantSet)
		f.checkRaceAudit(t, publishWon, wantSet)
		f.checkRaceDecision(t, publishWon)
	}
	t.Logf("outcomes over %d rounds: publish won %d, rollback won %d",
		rounds, wins[true], wins[false])
}
