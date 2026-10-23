package main

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A trimmed but structurally faithful copy of the IANA root-anchors document.
const sampleRootAnchors = `<?xml version="1.0" encoding="UTF-8"?>
<TrustAnchor id="0C05FDD6-422C-4910-8ED6-430ED15E11C2" source="http://data.iana.org/root-anchors/root-anchors.xml">
    <Zone>.</Zone>
    <KeyDigest id="Kjqmt7v" validFrom="2010-07-15T00:00:00+00:00" validUntil="2019-01-11T00:00:00+00:00">
        <KeyTag>19036</KeyTag>
        <Algorithm>8</Algorithm>
        <DigestType>2</DigestType>
        <Digest>49AAC11D7B6F6446702E54A1607371607A1A41855200FD2CE1CDDE32F24E8FB5</Digest>
    </KeyDigest>
    <KeyDigest id="Klajeyz" validFrom="2017-02-02T00:00:00+00:00">
        <KeyTag>20326</KeyTag>
        <Algorithm>8</Algorithm>
        <DigestType>2</DigestType>
        <Digest>E06D44B80B8F1D39A95C0B0D7C65D08458E880409BBC683457104237C7F8EC8D</Digest>
    </KeyDigest>
</TrustAnchor>`

// TestSnapshotRootAnchors verifies the document parses into the per-KeyDigest
// state we diff on, including whether the expiration (validUntil) attribute is
// present.
func TestSnapshotRootAnchors(t *testing.T) {
	ta, err := parseRootAnchorsForTest(t, sampleRootAnchors)
	if err != nil {
		t.Fatalf("parsing sample document: %s", err)
	}
	snap := snapshotRootAnchors(ta)

	if len(snap) != 2 {
		t.Fatalf("expected 2 KeyDigests, got %d", len(snap))
	}
	if got := snap["Kjqmt7v"]; got.keyTag != "19036" || got.validUntil == "" {
		t.Errorf("Kjqmt7v: expected keyTag 19036 with an expiration, got %+v", got)
	}
	if got := snap["Klajeyz"]; got.keyTag != "20326" || got.validUntil != "" {
		t.Errorf("Klajeyz: expected keyTag 20326 without an expiration, got %+v", got)
	}
}

// TestDiffRootAnchorsAddedDigest: a KeyDigest present now but not before is
// reported as added.
func TestDiffRootAnchorsAddedDigest(t *testing.T) {
	prev := map[string]rootAnchorState{
		"Klajeyz": {keyTag: "20326"},
	}
	cur := map[string]rootAnchorState{
		"Klajeyz": {keyTag: "20326"},
		"Kmyv6jo": {keyTag: "38696"},
	}

	got := diffRootAnchors(prev, cur).messages()
	if len(got) != 1 {
		t.Fatalf("expected exactly one issue, got %v", got)
	}
	want := `root-anchors.xml: new KeyDigest "Kmyv6jo" (KeyTag 38696) added`
	if got[0] != want {
		t.Errorf("expected %q, got %q", want, got[0])
	}
}

// TestDiffRootAnchorsGainedExpiration: an existing KeyDigest that gains a
// validUntil attribute is reported.
func TestDiffRootAnchorsGainedExpiration(t *testing.T) {
	prev := map[string]rootAnchorState{
		"Kjqmt7v": {keyTag: "19036"},
	}
	cur := map[string]rootAnchorState{
		"Kjqmt7v": {keyTag: "19036", validUntil: "2019-01-11T00:00:00+00:00"},
	}

	got := diffRootAnchors(prev, cur).messages()
	if len(got) != 1 {
		t.Fatalf("expected exactly one issue, got %v", got)
	}
	want := `root-anchors.xml: KeyDigest "Kjqmt7v" (KeyTag 19036) now has an expiration date (validUntil 2019-01-11T00:00:00+00:00)`
	if got[0] != want {
		t.Errorf("expected %q, got %q", want, got[0])
	}
}

// TestDiffRootAnchorsUnchanged: an identical document produces no issues, and a
// KeyDigest that already carried an expiration is not re-reported.
func TestDiffRootAnchorsUnchanged(t *testing.T) {
	state := map[string]rootAnchorState{
		"Kjqmt7v": {keyTag: "19036", validUntil: "2019-01-11T00:00:00+00:00"},
		"Klajeyz": {keyTag: "20326"},
	}

	if got := diffRootAnchors(state, state).messages(); len(got) != 0 {
		t.Fatalf("expected no issues on an unchanged document, got %v", got)
	}
}

// A copy of sampleRootAnchors with a third KeyDigest added, mirroring an IANA
// KSK rollover where a new key appears in the document.
const sampleRootAnchorsWithNewDigest = `<?xml version="1.0" encoding="UTF-8"?>
<TrustAnchor id="0C05FDD6-422C-4910-8ED6-430ED15E11C2" source="http://data.iana.org/root-anchors/root-anchors.xml">
    <Zone>.</Zone>
    <KeyDigest id="Kjqmt7v" validFrom="2010-07-15T00:00:00+00:00" validUntil="2019-01-11T00:00:00+00:00">
        <KeyTag>19036</KeyTag>
    </KeyDigest>
    <KeyDigest id="Klajeyz" validFrom="2017-02-02T00:00:00+00:00">
        <KeyTag>20326</KeyTag>
    </KeyDigest>
    <KeyDigest id="Kmyv6jo" validFrom="2024-07-18T00:00:00+00:00">
        <KeyTag>38696</KeyTag>
    </KeyDigest>
</TrustAnchor>`

// TestCheckRootAnchorsBaselineThenDiff exercises the full fetch path: the first
// cycle only establishes the baseline (no alert on existing KeyDigests), and a
// later document with a new KeyDigest is reported.
func TestCheckRootAnchorsBaselineThenDiff(t *testing.T) {
	body := sampleRootAnchors
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	// Isolate the package-level check state for this test.
	origURL, origSeen := conf.CheckRootAnchors, rootAnchorsSeen
	defer func() { conf.CheckRootAnchors, rootAnchorsSeen = origURL, origSeen }()
	conf.CheckRootAnchors = srv.URL
	rootAnchorsSeen = nil

	// First cycle: establish the baseline, report nothing for existing entries.
	if got := checkRootAnchors().messages(); len(got) != 0 {
		t.Fatalf("first cycle: expected baseline only (no issues), got %v", got)
	}

	// Second cycle, unchanged document: still nothing.
	if got := checkRootAnchors().messages(); len(got) != 0 {
		t.Fatalf("unchanged cycle: expected no issues, got %v", got)
	}

	// A new KeyDigest appears in the document.
	body = sampleRootAnchorsWithNewDigest
	got := checkRootAnchors().messages()
	if len(got) != 1 {
		t.Fatalf("changed cycle: expected exactly one issue, got %v", got)
	}
	want := `root-anchors.xml: new KeyDigest "Kmyv6jo" (KeyTag 38696) added`
	if got[0] != want {
		t.Errorf("changed cycle: expected %q, got %q", want, got[0])
	}
}

// TestCheckRootAnchorsDisabled: an empty config URL skips the check entirely.
func TestCheckRootAnchorsDisabled(t *testing.T) {
	origURL := conf.CheckRootAnchors
	defer func() { conf.CheckRootAnchors = origURL }()
	conf.CheckRootAnchors = ""

	if got := checkRootAnchors(); got != nil {
		t.Fatalf("expected no issues when disabled, got %v", got.messages())
	}
}

// parseRootAnchorsForTest parses raw XML into a rootAnchors, failing the test on
// error. It mirrors what fetchRootAnchors does after the HTTP fetch.
func parseRootAnchorsForTest(t *testing.T, raw string) (rootAnchors, error) {
	t.Helper()
	var ta rootAnchors
	err := xml.Unmarshal([]byte(raw), &ta)
	return ta, err
}
