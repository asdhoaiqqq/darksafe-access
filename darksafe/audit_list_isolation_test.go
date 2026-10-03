// This file pins down the independent-copy guarantee for the two lists
// embedded in an audited decision:
//
//   - Request.Subject.Roles — the subject's roles are part of the audited
//     request even though they take no part in organization policy
//     authorization, and their submitted order is historical fact.
//   - Decision.Matched — the complete matched-policy list is part of the
//     decision explanation an auditor must be able to reconstruct.
//
// The store promises to save its own detached copy when Decide is called
// and to hand out detached copies from audit query, paging, export,
// archive and recheck entry points. These tests exercise that promise
// through the public API only: after a decision the caller may rewrite or
// extend the request roles and matched list they hold, and after reading a
// record they may edit the same lists in every material they received;
// the stored history, a fresh read and a previously retained copy must all
// keep the decision-time content. A regression that aliases either list
// into caller memory fails here no matter which read path exposes it.
package darksafe

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// listIsolationRoles is a non-empty role list exercising Chinese, leading,
// trailing and internal spaces, and a lone non-UTF-8 byte. Roles never
// authorize anything, so the decision is driven by policies while the
// roles survive purely as preserved request content.
func listIsolationRoles() []string {
	return []string{"角色 alpha", "  前后 空格  ", "原" + invalidByte + "始"}
}

// listIsolationPolicies gives u2 exactly one allow hit (whose id carries a
// non-UTF-8 byte) and u1 a deny hit together with allow hits, so the deny
// decision still carries a complete, ordered matched list.
func listIsolationPolicies() []Policy {
	return []Policy{
		{ID: "z-allow 许", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
		{ID: "a-deny 拒", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny},
		{ID: "m-allow 中" + invalidByte, Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow, Recursive: true},
		{ID: "p-allow 通" + invalidByte, Subject: "u2", Action: "read", Scope: "org/a", Effect: EffectAllow},
	}
}

// roleRequest builds one legal request for the subject with the given
// roles, using a fresh backing array so each fixture request is independent.
func roleRequest(subject string, roles []string) OrgRequest {
	req := request("acme", subject, "r1", "org/a", "read")
	req.Subject.Roles = append([]string(nil), roles...)
	return req
}

// assertDecisionListContent checks the complete decision-time content the
// history must still show: role order, matched list, effect, reason and
// the version actually used.
func assertDecisionListContent(t *testing.T, r *AuditRecord, seq int, wantRoles []string, want Decision) {
	t.Helper()
	if r.Seq != seq || r.Kind != AuditDecision || r.Decision == nil {
		t.Fatalf("seq %d: not a decision record: %+v", seq, r)
	}
	if got := r.Decision.Request.Subject.Roles; !reflect.DeepEqual(got, wantRoles) {
		t.Fatalf("seq %d roles = %q, want decision-time %q", seq, got, wantRoles)
	}
	got := r.Decision.Decision
	if got.Allowed != want.Allowed {
		t.Fatalf("seq %d allowed = %v, want %v", seq, got.Allowed, want.Allowed)
	}
	if got.Reason != want.Reason {
		t.Fatalf("seq %d reason = %q, want %q", seq, got.Reason, want.Reason)
	}
	if got.Version != want.Version {
		t.Fatalf("seq %d version = %d, want %d", seq, got.Version, want.Version)
	}
	if !reflect.DeepEqual(got.Matched, want.Matched) {
		t.Fatalf("seq %d matched = %q, want %q", seq, got.Matched, want.Matched)
	}
}

// recordBySeq returns the decision record at seq, failing the test when it
// is absent or not a decision.
func recordBySeq(t *testing.T, recs []AuditRecord, seq int) *AuditRecord {
	t.Helper()
	for i := range recs {
		if recs[i].Seq == seq {
			if recs[i].Kind != AuditDecision || recs[i].Decision == nil {
				t.Fatalf("seq %d is not a decision", seq)
			}
			return &recs[i]
		}
	}
	t.Fatalf("no record at seq %d among %d records", seq, len(recs))
	return nil
}

// TestDecideListCopiesSurviveCallerEdits covers the primary promise: once
// Decide returned, the caller may replace role names in the request they
// submitted, append roles, and rewrite or extend the matched policies in
// the decision they got back. Both an allow decision and a deny decision
// that carries matched policies must keep independent copies — not only
// the authorization success. Every later read (export, paged query,
// online and offline recheck) must still show the decision-time role
// order, complete matched list, effect, reason and actual version.
func TestDecideListCopiesSurviveCallerEdits(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, listIsolationPolicies()); err != nil {
		t.Fatal(err)
	}
	roles := listIsolationRoles()
	allowReq := roleRequest("u2", roles)
	denyReq := roleRequest("u1", roles)

	allowD := s.Decide("acme", allowReq) // seq 2, allowed
	denyD := s.Decide("acme", denyReq)   // seq 3, denied with matches
	if !allowD.Allowed || allowD.Version != 1 || len(allowD.Matched) != 1 {
		t.Fatalf("setup allow decision = %+v", allowD)
	}
	if denyD.Allowed || denyD.Reason != "matched deny policy" ||
		denyD.Version != 1 || len(denyD.Matched) != 3 {
		t.Fatalf("setup deny decision = %+v, want a denial carrying 3 matches", denyD)
	}
	// Snapshot what history must remember before the caller edits anything.
	wantRoles := append([]string(nil), roles...)
	wantAllow := cloneDecisionValue(allowD)
	wantDeny := cloneDecisionValue(denyD)

	// The caller rewrites the request they submitted...
	allowReq.Subject.Roles[0] = "替换后的角色"
	allowReq.Subject.Roles = append(allowReq.Subject.Roles, "新增角色")
	denyReq.Subject.Roles[2] = "被改写的原始字节"
	denyReq.Subject.Roles = append(denyReq.Subject.Roles, "另一个新增角色")
	// ...and the returned decisions they hold.
	allowD.Matched[0] = "forged-policy"
	allowD.Matched = append(allowD.Matched, "ghost-policy")
	denyD.Matched[0] = "forged-policy"
	denyD.Matched = append(denyD.Matched, "ghost-policy")

	// Full export: stored content is byte-for-byte the decision-time content.
	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("records = %d, want 3; reads/edits must not append", len(recs))
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("stored chain changed through caller list edits: %v", err)
	}
	assertDecisionListContent(t, &recs[1], 2, wantRoles, wantAllow)
	assertDecisionListContent(t, &recs[2], 3, wantRoles, wantDeny)

	// Paged query returns the same content.
	paged := drainAudit(t, s, "acme", AuditDecision, "")
	if len(paged) != 2 {
		t.Fatalf("paged decisions = %d, want 2", len(paged))
	}
	assertDecisionListContent(t, recordBySeq(t, paged, 2), 2, wantRoles, wantAllow)
	assertDecisionListContent(t, recordBySeq(t, paged, 3), 3, wantRoles, wantDeny)

	// Replaying by the stored record still reaches the original conclusion.
	gotAllow, err := s.RecheckDecision("acme", 2)
	if err != nil || !reflect.DeepEqual(gotAllow, wantAllow) {
		t.Fatalf("online allow recheck = %+v, %v, want %+v", gotAllow, err, wantAllow)
	}
	gotDeny, err := s.RecheckDecision("acme", 3)
	if err != nil || !reflect.DeepEqual(gotDeny, wantDeny) {
		t.Fatalf("online deny recheck = %+v, %v, want %+v", gotDeny, err, wantDeny)
	}

	for _, seq := range []int{2, 3} {
		review, err := RecheckDecisionOffline("acme", recs, cp, seq)
		if err != nil {
			t.Fatalf("seq %d offline review: %v", seq, err)
		}
		if !review.Consistent {
			t.Fatalf("seq %d review inconsistent: %+v", seq, review)
		}
	}
	if rv := mustOfflineReview(t, recs, cp, 2); !reflect.DeepEqual(rv.Original, wantAllow) ||
		!reflect.DeepEqual(rv.Recomputed, wantAllow) {
		t.Fatalf("allow offline review = %+v, want %+v", rv, wantAllow)
	}
	if rv := mustOfflineReview(t, recs, cp, 3); !reflect.DeepEqual(rv.Original, wantDeny) ||
		!reflect.DeepEqual(rv.Recomputed, wantDeny) {
		t.Fatalf("deny offline review = %+v, want %+v", rv, wantDeny)
	}
}

// mustOfflineReview runs RecheckDecisionOffline, failing on error.
func mustOfflineReview(t *testing.T, recs []AuditRecord, cp Checkpoint, seq int) OfflineDecisionReview {
	t.Helper()
	rv, err := RecheckDecisionOffline("acme", recs, cp, seq)
	if err != nil {
		t.Fatalf("offline review seq %d: %v", seq, err)
	}
	return rv
}

// TestReadMaterialListEditsStayWithCaller edits the roles and matched
// lists in material obtained three different ways — the first query page,
// a later paged page, and a complete export. Each edit must change only
// that copy: another read and the copy retained earlier keep the original
// content, a fresh full export still verifies against the original
// checkpoint, and per-record rechecks still reach the original
// conclusions. A full export whose list content was edited must fail the
// original checkpoint with the existing ErrInvalidRange. None of these
// operations appends an audit record.
func TestReadMaterialListEditsStayWithCaller(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, listIsolationPolicies()); err != nil { // seq 1, v1
		t.Fatal(err)
	}
	roles := listIsolationRoles()
	allowReq := roleRequest("u2", roles)
	allowD := s.Decide("acme", allowReq) // seq 2, v1 allow
	wantAllow := cloneDecisionValue(allowD)
	if _, err := s.Publish("acme", 1, listIsolationPolicies()); err != nil { // seq 3, v2
		t.Fatal(err)
	}
	denyReq := roleRequest("u1", roles)
	denyD := s.Decide("acme", denyReq) // seq 4, v2 deny with matches
	wantDeny := cloneDecisionValue(denyD)
	s.Decide("acme", request("acme", "u9", "r1", "org/a", "read")) // seq 5, no match

	// A complete copy and the checkpoint retained before any caller edit.
	retained, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", retained, cp); err != nil {
		t.Fatalf("retained material must verify: %v", err)
	}
	wantRoles := append([]string(nil), roles...)

	// Edit the record inside the FIRST query page.
	page1, err := s.AuditQuery("acme", 1, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Records) != 2 || page1.Records[1].Seq != 2 {
		t.Fatalf("first page layout = %+v", page1.Records)
	}
	rewriteLists(&page1.Records[1])

	// Edit the record inside a LATER page.
	page2, err := s.AuditPage(page1.Checkpoint, page1.Next, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	page3, err := s.AuditPage(page2.Checkpoint, page2.Next, 2, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if page3.Next != 0 || len(page3.Records) != 1 || page3.Records[0].Seq != 5 {
		t.Fatalf("paging did not exhaust as expected: page2=%+v page3=%+v", page2, page3)
	}
	rewriteLists(recordBySeq(t, page2.Records, 4))

	// Edit a complete export copy.
	mine, _, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	rewriteLists(recordBySeq(t, mine, 2))
	rewriteLists(recordBySeq(t, mine, 4))

	// A full export whose list content was edited cannot pass the original
	// checkpoint, and offline review must refuse it rather than review
	// tampered history.
	tampered := cloneExportedRecords(retained)
	rewriteLists(recordBySeq(t, tampered, 2))
	rewriteLists(recordBySeq(t, tampered, 4))
	if err := VerifyAudit("acme", tampered, cp); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("tampered export verify err = %v, want ErrInvalidRange", err)
	}
	for _, seq := range []int{2, 4} {
		if rv, err := RecheckDecisionOffline("acme", tampered, cp, seq); !errors.Is(err, ErrInvalidRange) {
			t.Fatalf("tampered offline review seq %d = %+v, %v, want ErrInvalidRange", seq, rv, err)
		}
	}

	// A fresh read still shows decision-time content...
	fresh, freshCP, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if freshCP != cp {
		t.Fatalf("checkpoint changed after caller edits: %+v vs %+v", freshCP, cp)
	}
	if err := VerifyAudit("acme", fresh, cp); err != nil {
		t.Fatalf("fresh export fails the original checkpoint: %v", err)
	}
	// ...the previously retained copy is untouched...
	if !reflect.DeepEqual(fresh, retained) {
		t.Fatal("retained material diverged after other copies were edited")
	}
	// ...and a fresh paged query agrees as well.
	if got := drainAudit(t, s, "acme", "", ""); !reflect.DeepEqual(got, retained) {
		t.Fatal("paged query content changed after page copies were edited")
	}
	assertDecisionListContent(t, recordBySeq(t, fresh, 2), 2, wantRoles, wantAllow)
	assertDecisionListContent(t, recordBySeq(t, fresh, 4), 4, wantRoles, wantDeny)

	// Per-record rechecks still reach the original conclusions.
	if got, err := s.RecheckDecision("acme", 2); err != nil || !reflect.DeepEqual(got, wantAllow) {
		t.Fatalf("online recheck seq 2 = %+v, %v, want %+v", got, err, wantAllow)
	}
	if got, err := s.RecheckDecision("acme", 4); err != nil || !reflect.DeepEqual(got, wantDeny) {
		t.Fatalf("online recheck seq 4 = %+v, %v, want %+v", got, err, wantDeny)
	}
	if rv := mustOfflineReview(t, fresh, cp, 2); !rv.Consistent ||
		!reflect.DeepEqual(rv.Original, wantAllow) {
		t.Fatalf("fresh offline review seq 2 = %+v, want %+v", rv, wantAllow)
	}
	if rv := mustOfflineReview(t, fresh, cp, 4); !rv.Consistent ||
		!reflect.DeepEqual(rv.Original, wantDeny) {
		t.Fatalf("fresh offline review seq 4 = %+v, want %+v", rv, wantDeny)
	}

	// All of the above is read-only: no audit record was appended.
	if got := len(drainAudit(t, s, "acme", "", "")); got != 5 {
		t.Fatalf("records = %d, want 5; reads and edits must not append", got)
	}
	if v := s.CurrentVersion("acme"); v != 2 {
		t.Fatalf("current version = %d, want 2", v)
	}
}

// rewriteLists simulates the caller editing one record copy: overwrite a
// role name and a matched policy name, then append entries to both lists.
// Lists must be non-empty, which every target decision carries.
func rewriteLists(r *AuditRecord) {
	roles := r.Decision.Request.Subject.Roles
	roles[0] = "forged-role"
	r.Decision.Request.Subject.Roles = append(roles, "ghost-role")
	matched := r.Decision.Decision.Matched
	matched[0] = "forged-policy"
	r.Decision.Decision.Matched = append(matched, "ghost-policy")
}

// TestEmptyListShapesAndMatchlessDenialsPreserved locks the current
// storage shapes for empty lists: a request with no roles (nil) and one
// with an explicitly empty role list must not be exchanged for each other
// on read, and denials that matched no policy — both the "no matching
// allow policy" result (non-nil empty matched list) and an envelope
// rejection before policy evaluation (nil matched list, version 0) — must
// keep their original result shapes through export, paging and archive.
func TestEmptyListShapesAndMatchlessDenialsPreserved(t *testing.T) {
	s := NewStore()
	if _, err := s.Publish("acme", 0, nil); err != nil { // v1 denies everything
		t.Fatal(err)
	}

	nilReq := request("acme", "u1", "r1", "org/a", "read") // roles left nil
	nilDeny := s.Decide("acme", nilReq)                     // seq 2
	emptyReq := request("acme", "u2", "r1", "org/a", "read")
	emptyReq.Subject.Roles = []string{} // explicitly empty
	emptyDeny := s.Decide("acme", emptyReq) // seq 3
	disabledReq := request("acme", "u3", "r1", "org/a", "read")
	disabledReq.Subject.Disabled = true
	envDeny := s.Decide("acme", disabledReq) // seq 4, envelope rejection

	for _, d := range []Decision{nilDeny, emptyDeny} {
		if d.Allowed || d.Reason != "no matching allow policy" || d.Version != 1 || len(d.Matched) != 0 {
			t.Fatalf("setup policy denial = %+v", d)
		}
	}
	if envDeny.Allowed || envDeny.Reason != "subject is disabled" || envDeny.Version != 0 || envDeny.Matched != nil {
		t.Fatalf("setup envelope denial = %+v, want nil matched, version 0", envDeny)
	}

	// A late edit of the caller's explicitly-empty slice must not matter.
	emptyReq.Subject.Roles = append(emptyReq.Subject.Roles, "late-role")

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("empty-list chain: %v", err)
	}
	nilRec := recordBySeq(t, recs, 2)
	emptyRec := recordBySeq(t, recs, 3)
	envRec := recordBySeq(t, recs, 4)

	if nilRec.Decision.Request.Subject.Roles != nil {
		t.Fatalf("nil roles read back as %v", nilRec.Decision.Request.Subject.Roles)
	}
	if roles := emptyRec.Decision.Request.Subject.Roles; roles == nil || len(roles) != 0 {
		t.Fatalf("explicitly empty roles read back as %v", roles)
	}
	// The two shapes must not collapse onto one fingerprint.
	if nilRec.Fingerprint == emptyRec.Fingerprint {
		t.Fatal("nil roles and empty roles share a fingerprint")
	}
	// Policy-evaluation denials carry a non-nil empty matched list; the
	// envelope rejection carries a nil one. Both are matchless denials.
	if nilRec.Decision.Decision.Matched == nil || emptyRec.Decision.Decision.Matched == nil {
		t.Fatal("no-match denial must keep its non-nil empty matched list")
	}
	if envRec.Decision.Decision.Matched != nil {
		t.Fatal("envelope rejection matched list must stay nil")
	}
	if !reflect.DeepEqual(nilRec.Decision.Decision, cloneDecisionValue(nilDeny)) {
		t.Fatalf("nil-roles denial not preserved: %+v", nilRec.Decision.Decision)
	}
	if !reflect.DeepEqual(emptyRec.Decision.Decision, cloneDecisionValue(emptyDeny)) {
		t.Fatalf("empty-roles denial not preserved: %+v", emptyRec.Decision.Decision)
	}

	// The paged query keeps the same shapes.
	paged := drainAudit(t, s, "acme", AuditDecision, "")
	if got := recordBySeq(t, paged, 2).Decision.Request.Subject.Roles; got != nil {
		t.Fatalf("paged query turned nil roles into %v", got)
	}
	if got := recordBySeq(t, paged, 3).Decision.Request.Subject.Roles; got == nil || len(got) != 0 {
		t.Fatalf("paged query turned empty roles into %v", got)
	}

	// The archive round trip keeps the same shapes and still verifies.
	archive, err := EncodeAuditArchive("acme", recs, cp)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("archive decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, recs) {
		t.Fatal("archive round trip changed empty-list shapes")
	}
	if err := VerifyAudit("acme", decoded, cp); err != nil {
		t.Fatalf("decoded empty-list chain: %v", err)
	}

	// Every matchless denial re-reviews to the original conclusion.
	for seq, want := range map[int]Decision{2: nilDeny, 3: emptyDeny, 4: envDeny} {
		if got, err := s.RecheckDecision("acme", seq); err != nil || !decisionsEqual(got, want) {
			t.Fatalf("online recheck seq %d = %+v, %v, want %+v", seq, got, err, want)
		}
		if rv := mustOfflineReview(t, recs, cp, seq); !rv.Consistent {
			t.Fatalf("matchless denial seq %d review inconsistent: %+v", seq, rv)
		}
	}
}

// TestNonEmptyListsPreserveChineseSpacesAndRawBytes checks the byte-level
// content guarantee on non-empty lists: Chinese, spaces and non-UTF-8
// bytes in roles and matched policy ids survive as the original bytes,
// even after the caller edits the request, the returned decision and
// later read copies. The full export keeps passing its chain check,
// including through an archive round trip.
func TestNonEmptyListsPreserveChineseSpacesAndRawBytes(t *testing.T) {
	s := NewStore()
	policies := []Policy{
		// Allow hit with spaces and Chinese; deny hit carrying a different
		// invalid byte than the request does, so matched ordering exercises
		// raw content too.
		{ID: "通 行 策 略", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectAllow},
		{ID: "拒" + anotherInvalidByte + "止 策 略", Subject: "u1", Action: "read", Scope: "org/a", Effect: EffectDeny},
		{ID: "允 许" + invalidByte + "策 略", Subject: "u2", Action: "read", Scope: "org/a", Effect: EffectAllow},
	}
	if _, err := s.Publish("acme", 0, policies); err != nil {
		t.Fatal(err)
	}
	roles := listIsolationRoles()
	allowReq := roleRequest("u2", roles)
	denyReq := roleRequest("u1", roles)
	allowD := s.Decide("acme", allowReq) // seq 2
	denyD := s.Decide("acme", denyReq)   // seq 3
	if !allowD.Allowed || denyD.Allowed {
		t.Fatalf("setup decisions: allow=%+v deny=%+v", allowD, denyD)
	}
	if !strings.Contains(allowD.Matched[0], invalidByte) {
		t.Fatal("test premise broken: allow hit must carry the invalid byte")
	}
	if !strings.Contains(denyD.Matched[0], anotherInvalidByte) {
		t.Fatal("test premise broken: deny hit must carry the other invalid byte")
	}
	wantRoles := append([]string(nil), roles...)
	wantAllow := cloneDecisionValue(allowD)
	wantDeny := cloneDecisionValue(denyD)

	// Caller edits the submitted request lists and the returned decisions.
	allowReq.Subject.Roles[0] = "plain replacement"
	allowReq.Subject.Roles = append(allowReq.Subject.Roles, "尾随角色")
	denyReq.Subject.Roles[2] = "\xfe"
	denyReq.Subject.Roles = append(denyReq.Subject.Roles, "x")
	allowD.Matched[0] = "plain"
	allowD.Matched = append(allowD.Matched, "ghost")
	denyD.Matched[0] = "plain"
	denyD.Matched = append(denyD.Matched, "ghost")

	recs, cp, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAudit("acme", recs, cp); err != nil {
		t.Fatalf("raw-byte chain after caller edits: %v", err)
	}
	assertDecisionListContent(t, &recs[1], 2, wantRoles, wantAllow)
	assertDecisionListContent(t, &recs[2], 3, wantRoles, wantDeny)
	savedRoles := recs[1].Decision.Request.Subject.Roles
	if !strings.Contains(savedRoles[0], "角色") ||
		!strings.HasPrefix(savedRoles[1], "  ") || !strings.HasSuffix(savedRoles[1], "  ") ||
		!strings.Contains(savedRoles[2], invalidByte) {
		t.Fatalf("roles lost their original bytes/spacing/order: %q", savedRoles)
	}
	if !strings.Contains(recordBySeq(t, recs, 2).Decision.Decision.Matched[0], invalidByte) {
		t.Fatal("stored allow matched list lost its invalid byte")
	}
	if !strings.Contains(recordBySeq(t, recs, 3).Decision.Decision.Matched[0], anotherInvalidByte) {
		t.Fatal("stored deny matched list lost its invalid byte")
	}

	// Editing the exported copy and reading again leaves the stored bytes
	// intact and the chain check still holds on the fresh read.
	rewriteLists(&recs[1])
	rewriteLists(&recs[2])
	fresh, freshCP, err := s.AuditExport("acme", 0)
	if err != nil {
		t.Fatal(err)
	}
	if freshCP != cp {
		t.Fatal("checkpoint changed despite list edits")
	}
	if err := VerifyAudit("acme", fresh, cp); err != nil {
		t.Fatalf("fresh raw-byte export must still verify: %v", err)
	}
	assertDecisionListContent(t, &fresh[1], 2, wantRoles, wantAllow)
	assertDecisionListContent(t, &fresh[2], 3, wantRoles, wantDeny)

	// Paged query bytes agree with the export's decision records.
	if got := drainAudit(t, s, "acme", AuditDecision, ""); !reflect.DeepEqual(got, fresh[1:]) {
		t.Fatal("paged raw-byte content differs from the export")
	}

	// The archive preserves the exact bytes offline and stays verifiable.
	archive, err := EncodeAuditArchive("acme", fresh, cp)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAuditArchive(archive, "acme", cp)
	if err != nil {
		t.Fatalf("raw-byte archive decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, fresh) {
		t.Fatal("raw-byte archive round trip changed list content")
	}

	// Offline re-computation derives matched policy ids with the same raw
	// bytes and agrees with the preserved originals.
	allowReview := mustOfflineReview(t, decoded, cp, 2)
	if !allowReview.Consistent || !reflect.DeepEqual(allowReview.Original, wantAllow) {
		t.Fatalf("raw-byte allow review = %+v, want %+v", allowReview, wantAllow)
	}
	if !strings.Contains(allowReview.Recomputed.Matched[0], invalidByte) {
		t.Fatal("offline recomputation normalized the allow matched id")
	}
	denyReview := mustOfflineReview(t, decoded, cp, 3)
	if !denyReview.Consistent || !reflect.DeepEqual(denyReview.Original, wantDeny) {
		t.Fatalf("raw-byte deny review = %+v, want %+v", denyReview, wantDeny)
	}
	if !strings.Contains(denyReview.Recomputed.Matched[0], anotherInvalidByte) {
		t.Fatal("offline recomputation normalized the deny matched id")
	}

	// Nothing here appended records.
	if got := len(drainAudit(t, s, "acme", "", "")); got != 3 {
		t.Fatalf("records = %d, want 3; reads and edits must not append", got)
	}
}
