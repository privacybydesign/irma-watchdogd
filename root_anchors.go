package main

import (
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
)

// maxRootAnchorsBytes bounds how much of the root-anchors document we read. The
// real file is a couple of kilobytes; the limit guards against a misbehaving or
// hostile endpoint returning an unbounded body.
const maxRootAnchorsBytes = 1 << 20 // 1 MiB

// rootAnchors mirrors the structure of the IANA DNSSEC root trust anchors
// document at https://data.iana.org/root-anchors/root-anchors.xml. Only the
// fields we diff on are modelled.
type rootAnchors struct {
	XMLName    xml.Name    `xml:"TrustAnchor"`
	KeyDigests []keyDigest `xml:"KeyDigest"`
}

type keyDigest struct {
	ID string `xml:"id,attr"`
	// ValidUntil is the document's expiration attribute for a KeyDigest: when it
	// is present the entry is scheduled to be retired. A KeyDigest that gains it
	// is what the issue calls "gaining an expiration attribute".
	ValidUntil string `xml:"validUntil,attr"`
	KeyTag     string `xml:"KeyTag"`
}

// rootAnchorState is the minimal per-KeyDigest state we keep between check
// cycles so we can detect meaningful changes to the document.
type rootAnchorState struct {
	keyTag     string
	validUntil string
}

// rootAnchorsSeen is the baseline the current document is diffed against. It is
// nil until the first successful fetch and, like the rest of the watchdog's
// state (see the debounce maps in main.go), lives in memory: a restart re-reads
// the current document as the new baseline. It is only ever touched from the
// single background check goroutine.
var rootAnchorsSeen map[string]rootAnchorState

// snapshotRootAnchors reduces a parsed document to the per-KeyDigest state we
// persist between cycles, keyed by the KeyDigest id attribute.
func snapshotRootAnchors(ta rootAnchors) map[string]rootAnchorState {
	m := make(map[string]rootAnchorState, len(ta.KeyDigests))
	for _, kd := range ta.KeyDigests {
		m[kd.ID] = rootAnchorState{keyTag: kd.KeyTag, validUntil: kd.ValidUntil}
	}
	return m
}

// diffRootAnchors reports the two conditions the check watches for: a KeyDigest
// present in cur but not in prev (a newly added entry), and a KeyDigest that
// carries a validUntil (expiration) attribute in cur but did not in prev.
// Removed entries and other attribute changes are intentionally not reported.
func diffRootAnchors(prev, cur map[string]rootAnchorState) issueEntries {
	ids := make([]string, 0, len(cur))
	for id := range cur {
		ids = append(ids, id)
	}
	sort.Strings(ids) // stable output regardless of map iteration order

	var ret issueEntries
	for _, id := range ids {
		c := cur[id]
		p, existed := prev[id]
		if !existed {
			ret = append(ret, issueEntry{warning, fmt.Sprintf(
				"root-anchors.xml: new KeyDigest %q (KeyTag %s) added", id, c.keyTag)})
			continue
		}
		if c.validUntil != "" && p.validUntil == "" {
			ret = append(ret, issueEntry{warning, fmt.Sprintf(
				"root-anchors.xml: KeyDigest %q (KeyTag %s) now has an expiration date (validUntil %s)",
				id, c.keyTag, c.validUntil)})
		}
	}
	return ret
}

// fetchRootAnchors retrieves and parses the root-anchors document at url.
func fetchRootAnchors(url string) (rootAnchors, error) {
	client := newHTTPClient()
	resp, err := client.Get(url)
	if err != nil {
		return rootAnchors{}, fmt.Errorf("error fetching: %s", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return rootAnchors{}, fmt.Errorf("unexpected status code %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRootAnchorsBytes))
	if err != nil {
		return rootAnchors{}, fmt.Errorf("reading body: %s", err)
	}

	var ta rootAnchors
	if err := xml.Unmarshal(body, &ta); err != nil {
		return rootAnchors{}, fmt.Errorf("parsing XML: %s", err)
	}
	return ta, nil
}

// checkRootAnchors fetches the configured root-anchors document and reports when
// a KeyDigest is newly added or newly gains an expiration date. The first
// successful fetch only establishes the baseline, so the KeyDigests already
// present at startup are not reported.
func checkRootAnchors() (ret issueEntries) {
	url := conf.CheckRootAnchors
	if url == "" {
		return
	}

	log.Printf(" checking root anchors %s", url)

	ta, err := fetchRootAnchors(url)
	if err != nil {
		return issueEntries{{warning, fmt.Sprintf("root-anchors.xml: %s", err)}}
	}

	cur := snapshotRootAnchors(ta)
	if rootAnchorsSeen == nil {
		rootAnchorsSeen = cur
		return
	}

	// The baseline is deliberately not advanced: a new or newly-expiring
	// KeyDigest stays reported on every cycle (so it survives the cross-cycle
	// debounce in confirmIssues) until the change is no longer present, at which
	// point it is reported fixed like any other issue.
	return diffRootAnchors(rootAnchorsSeen, cur)
}
