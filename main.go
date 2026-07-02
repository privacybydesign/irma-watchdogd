// (c) 2017 - Bas Westerbaan <bas@westerbaan.name>
// You may redistribute this file under the conditions of the GPLv3.

// irma-watchdogd is a simple webserver that checks various properties of
// the public irma infrastructure.

package main

import (
	"context"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	irma "github.com/privacybydesign/irmago"

	"github.com/ashwanthkumar/slack-go-webhook"
	"github.com/dustin/go-humanize"
	"gopkg.in/yaml.v3"

	"github.com/bwesterb/go-atum"
)

var exampleConfig string = `
    checkschememanagers:
        https://privacybydesign.foundation/schememanager/pbdf:
            |
                -----BEGIN PUBLIC KEY-----
                MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAELzHV5ipBimWpuZIDaQQd+KmNpNop
                dpBeCqpDwf+Grrw9ReODb6nwlsPJ/c/gqLnc+Y3sKOAJ2bFGI+jHBSsglg==
                -----END PUBLIC KEY-----
    bindaddr: ':8080'
    interval: 5m `

var rawTemplate string = `
<html>
    <head>
        <title>irma watchdog</title>
        <style>
        body {
            color: white;
            background-color: black;
            font-family: Open Sans,Helvetica,Arial,sans-serif;
            font-size: smaller;
        }
        </style>
    </head>
    <body>
        <ul>
        {{ range $i, $issue := .Issues }}
            <li>{{ $issue }}</li>
        {{ else }}
            <li>Everything is ok!</li>
        {{ end }}
        </ul>
        <p>Last update {{ .LastCheck }}</p>
        <script type="text/javascript">
            setTimeout(function() {
                window.location.reload(1);
            }, {{ .Interval }});
        </script>
    </body>
</html>`

type templateContext struct {
	Issues    []string
	Interval  int
	LastCheck string
}

// Globals
var (
	conf           Conf
	ticker         *time.Ticker
	initialCheck   bool
	parsedTemplate *template.Template

	// failureStreaks tracks, per issue message, how many consecutive check
	// cycles it has been observed. Used to debounce transient blips.
	failureStreaks = map[string]int{}

	// recoveryStreaks tracks, per confirmed issue message, how many consecutive
	// check cycles it has been ABSENT. It mirrors failureStreaks for the recovery
	// direction: a confirmed issue is only reported fixed once its recovery streak
	// reaches conf.FailureThreshold, so a genuine outage that briefly flaps to OK
	// for a cycle or two is not prematurely reported as fixed and then re-alerted.
	recoveryStreaks = map[string]int{}

	// confirmedSet holds the currently-confirmed (reported) issues, keyed by
	// message. An issue enters this set once its failure streak reaches the
	// threshold and only leaves it once its recovery streak reaches the threshold;
	// while it is recovering it stays in the set so the status page and alerts stay
	// stable. This is the debounced set the new/fixed diff runs against.
	confirmedSet = map[string]issueEntry{}

	// cycleCount counts how many check cycles have run since (re)start. It drives
	// the initialCheck window: an issue present from the very first cycle is only
	// confirmed once its streak reaches conf.FailureThreshold (i.e. at cycle
	// conf.FailureThreshold), so the "this might be pre-existing" startup window
	// must stay open that long rather than closing after the first cycle.
	cycleCount int

	// stateMu guards the mutable state shared between the background check
	// goroutine (writer) and the HTTP handler (reader). Without it the handler
	// races the checker on every cycle, which can produce a torn read and crash
	// the server. Access lastCheck/issues only through stateMu / setState.
	stateMu   sync.RWMutex
	lastCheck time.Time
	issues    issueEntries
)

// setState atomically publishes the result of a completed check cycle.
func setState(curIssues issueEntries, when time.Time) {
	stateMu.Lock()
	defer stateMu.Unlock()
	issues = curIssues
	lastCheck = when
}

// currentState returns a snapshot of the published state for rendering.
func currentState() (issueEntries, time.Time) {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return issues, lastCheck
}

// Configuration
type Conf struct {
	CheckSchemeManagers    map[string]string // {url: pk}
	BindAddr               string            // port to bind to
	CheckCertificateExpiry []string
	CheckAtumServers       []string
	HealthChecks           []HealthCheck
	Interval               time.Duration
	SlackWebhooks          []string
	WebHooks               []string
	FailureThreshold       int // consecutive cycles an issue must persist before it is reported (and be absent before it is reported fixed)
}

func main() {
	var confPath string

	// set configuration defaults
	conf.BindAddr = ":8080"
	conf.Interval = 5 * time.Minute
	conf.FailureThreshold = 3

	// parse commandline
	flag.StringVar(&confPath, "config", "config.yaml",
		"Path to configuration file")
	flag.Parse()

	// parse configuration file
	if _, err := os.Stat(confPath); os.IsNotExist(err) {
		fmt.Printf("Could not find config file: %s\n", confPath)
		fmt.Println("It should look something like")
		fmt.Println(exampleConfig)
		os.Exit(1)
		return
	} else if err != nil {
		log.Fatalf("Could not stat configuration file: %v", err)
	}

	buf, err := os.ReadFile(confPath)
	if err != nil {
		log.Fatalf("Could not read config file %s: %s", confPath, err)
	}

	if err := yaml.Unmarshal(buf, &conf); err != nil {
		log.Fatalf("Could not parse config file: %s", err)
	}

	// A threshold below 1 would mean "report before observing", which makes no
	// sense; clamp it so a value of 1 reproduces the old alert-immediately behaviour.
	if conf.FailureThreshold < 1 {
		conf.FailureThreshold = 1
	}

	// Load IRMA configuration
	tempDir, err := os.MkdirTemp("", "")
	if err != nil {
		log.Printf("checkSchemeManager: TempDir: %s", err)
		return
	}
	defer os.RemoveAll(tempDir)

	icDir := path.Join(tempDir, "irma_configuration")
	err = os.Mkdir(icDir, 0700)
	if err != nil {
		log.Printf("MkDir in temp dir for IRMA configuration (%s): %s", icDir, err)
		return
	}
	irmaConfig, err := irma.NewConfiguration(icDir, irma.ConfigurationOptions{})
	if err != nil {
		log.Printf("IRMA configuration could not be loaded in temp dir %s: %s", icDir, err)
		return
	}
	for url, pk := range conf.CheckSchemeManagers {
		if err = irmaConfig.InstallScheme(url, []byte(pk)); err != nil {
			log.Printf("could not install scheme %s: %s", icDir, err)
			return
		}
	}

	// set up HTTP server
	http.HandleFunc("/", handler)

	// parse template
	parsedTemplate, err = template.New("template").Parse(rawTemplate)
	if err != nil {
		panic(err)
	}

	log.Printf("Will check status every %s", conf.Interval)
	ticker = time.NewTicker(conf.Interval)

	go func() {
		for {
			runChecks(irmaConfig)
			<-ticker.C
		}
	}()

	log.Printf("Listening on %s", conf.BindAddr)

	log.Fatal(http.ListenAndServe(conf.BindAddr, nil))
}

// Handle / HTTP request
func handler(w http.ResponseWriter, r *http.Request) {
	curIssues, when := currentState()
	err := parsedTemplate.Execute(w, templateContext{
		LastCheck: humanize.Time(when),
		Issues:    curIssues.messages(),
		Interval:  int(conf.Interval.Seconds() * 1000),
	})
	if err != nil {
		log.Printf("Error executing template: %s", err)
	}
}

// Computes difference between old and new issues
func difference(old, cur issueEntries) (came, gone issueEntries) {
	lut := make(map[string]bool)
	for _, x := range old {
		lut[x.message] = true
	}
	for _, x := range cur {
		if _, ok := lut[x.message]; !ok {
			came = append(came, x)
		} else {
			lut[x.message] = false
		}
	}
	for _, x := range old {
		isGone := lut[x.message]
		if isGone {
			gone = append(gone, x)
		}
	}
	return
}

func runChecks(irmaConfig *irma.Configuration) {
	var curIssues issueEntries

	// An issue that has been failing since startup is only confirmed once its
	// streak reaches conf.FailureThreshold, which happens on cycle
	// conf.FailureThreshold. Treat every cycle up to and including that one as
	// "initial", so a pre-existing outage that surfaces under debouncing is still
	// recognised as a restart artefact (suppressed from webhooks, flagged with
	// the "I just (re)started" Slack preamble) rather than paged as brand new.
	cycleCount++
	initialCheck = cycleCount <= conf.FailureThreshold

	log.Println("Running checks ...")
	curIssues = append(curIssues, checkSchemeManagers(irmaConfig)...)
	curIssues = append(curIssues, checkCertificateExpiry()...)
	curIssues = append(curIssues, checkAtumServers()...)
	curIssues = append(curIssues, runHealthChecks(conf.HealthChecks)...)

	logCurrentIssues(curIssues.messages())

	// Debounce transient blips in both directions: an issue is only treated as
	// confirmed once it has been observed for conf.FailureThreshold consecutive
	// cycles, and a confirmed issue is only treated as fixed once it has been
	// absent for that many consecutive cycles. A host that is briefly unreachable
	// (or a confirmed outage that briefly flaps to OK) within a cycle or two
	// therefore produces no alert churn at all.
	confirmedIssues := confirmIssues(curIssues)

	prevIssues, _ := currentState()
	newIssues, fixedIssues := difference(prevIssues, confirmedIssues)

	if len(conf.SlackWebhooks) > 0 {
		go pushToSlack(newIssues, fixedIssues, initialCheck)
	}

	// If this is an initial check, don't send the issues to webhooks
	if len(conf.WebHooks) > 0 && !initialCheck {
		go pushToWebHooks(newIssues)
	}

	setState(confirmedIssues, time.Now())
}

// confirmIssues applies symmetric cross-cycle debouncing and returns the current
// confirmed set. An issue is confirmed (and thus reported as new) once it has
// been observed for conf.FailureThreshold consecutive cycles; a confirmed issue
// is only dropped (and thus reported as fixed) once it has been absent for that
// many consecutive cycles. While an issue is recovering it stays in the set, so
// neither a transient failure blip nor a transient recovery blip produces alert
// churn. The full confirmed set is returned each cycle; runChecks diffs it
// against the previous confirmed set to derive the new/fixed alerts.
func confirmIssues(curIssues issueEntries) (confirmed issueEntries) {
	// De-duplicate the current cycle's messages (keeping the first entry per
	// message), so two entries with the same message (e.g. duplicate health
	// checks on one URL) can't advance a streak twice or land in confirmed twice.
	curEntries := make(map[string]issueEntry, len(curIssues))
	var order []string
	for _, issue := range curIssues {
		if _, seen := curEntries[issue.message]; seen {
			continue
		}
		curEntries[issue.message] = issue
		order = append(order, issue.message)
	}

	var pending, recovering []string

	// Present issues: clear any recovery streak and advance the failure streak.
	// Confirm once the threshold is reached; refresh the stored entry for issues
	// that are already confirmed (their level may have changed).
	for _, msg := range order {
		delete(recoveryStreaks, msg)
		if _, ok := confirmedSet[msg]; ok {
			confirmedSet[msg] = curEntries[msg]
			continue
		}
		failureStreaks[msg]++
		if failureStreaks[msg] >= conf.FailureThreshold {
			confirmedSet[msg] = curEntries[msg]
		} else {
			pending = append(pending, fmt.Sprintf("%s (%d/%d)", msg, failureStreaks[msg], conf.FailureThreshold))
		}
	}

	// Unconfirmed issues that are absent this cycle: a blip that never reached the
	// threshold and is now gone; reset its failure streak so it counts from
	// scratch if it ever returns.
	for msg := range failureStreaks {
		if _, present := curEntries[msg]; present {
			continue
		}
		if _, ok := confirmedSet[msg]; ok {
			continue
		}
		delete(failureStreaks, msg)
	}

	// Confirmed issues that are absent this cycle: advance the recovery streak and
	// only drop them from the confirmed set (reporting them fixed) once it reaches
	// the threshold, mirroring the failure debounce.
	for msg := range confirmedSet {
		if _, present := curEntries[msg]; present {
			continue
		}
		recoveryStreaks[msg]++
		if recoveryStreaks[msg] >= conf.FailureThreshold {
			delete(confirmedSet, msg)
			delete(failureStreaks, msg)
			delete(recoveryStreaks, msg)
		} else {
			recovering = append(recovering, fmt.Sprintf("%s (%d/%d)", msg, recoveryStreaks[msg], conf.FailureThreshold))
		}
	}

	if len(pending) > 0 {
		log.Printf("Pending issues (awaiting confirmation):\n%s", strings.Join(pending, "\n"))
	}
	if len(recovering) > 0 {
		log.Printf("Recovering issues (awaiting fixed confirmation):\n%s", strings.Join(recovering, "\n"))
	}

	// Return the full confirmed set: current-cycle entries first (in detection
	// order) for stable output, then any confirmed issues that are recovering.
	for _, msg := range order {
		if entry, ok := confirmedSet[msg]; ok {
			confirmed = append(confirmed, entry)
		}
	}
	for msg, entry := range confirmedSet {
		if _, present := curEntries[msg]; present {
			continue
		}
		confirmed = append(confirmed, entry)
	}

	return
}

func pushToWebHooks(newIssues issueEntries) {
	dangers := newIssues.filter(danger)
	for _, msg := range dangers {
		for _, bareURL := range conf.WebHooks {
			u := fmt.Sprintf(bareURL, url.QueryEscape("Watchdog: "+msg))
			res, err := http.Get(u)
			if err != nil {
				log.Printf("Webhook %s: %s", u, err)
				return
			}
			body, err := io.ReadAll(res.Body)
			if err != nil {
				log.Printf("Webhook response body error: %s", err)
				return
			}
			if len(body) != 0 {
				log.Printf("Webhook response body: %s", string(body))
			}
		}
	}
}

func pushToSlack(newIssues, fixedIssues issueEntries, initial bool) {
	strGood := "good"
	strWarning := "warning"
	strBad := "bad"
	if len(newIssues) > 0 {
		if initial {
			pushMessageToSlack("I just (re)started, so I might repeat some known issues.", []slack.Attachment{})
		}

		dangers := newIssues.filter(danger)
		warnings := newIssues.filter(warning)

		if len(dangers) > 0 {
			// Add mention such that notifications for warnings can be suppressed.
			message := "<!channel> New issues discovered."
			var attachments []slack.Attachment
			for _, msg := range dangers {
				msg := msg
				attachments = append(attachments, slack.Attachment{
					Fallback: &msg,
					Text:     &msg,
					Color:    &strBad,
				})
			}
			pushMessageToSlack(message, attachments)
		}

		if len(warnings) > 0 {
			message := "New warnings discovered."
			var attachments []slack.Attachment
			for _, msg := range warnings {
				msg := msg
				attachments = append(attachments, slack.Attachment{
					Fallback: &msg,
					Text:     &msg,
					Color:    &strWarning,
				})
			}
			pushMessageToSlack(message, attachments)
		}
	}

	if len(fixedIssues) > 0 {
		message := "The following issues and warnings were fixed."
		var attachments []slack.Attachment
		for _, msg := range fixedIssues.messages() {
			msg := msg
			attachments = append(attachments, slack.Attachment{
				Fallback: &msg,
				Text:     &msg,
				Color:    &strGood,
			})
		}
		pushMessageToSlack(message, attachments)
	}
}

func pushMessageToSlack(message string, attachments []slack.Attachment) {
	for _, url := range conf.SlackWebhooks {
		payload := slack.Payload{
			Text:        message,
			Username:    "irma-watchdogd",
			IconEmoji:   ":dog:",
			Attachments: attachments,
		}
		if err := slack.Send(url, "", payload); err != nil {
			log.Printf("SlackWebhook %s: %s", url, err)
			continue
		}
	}
}

func logCurrentIssues(curIssues []string) {
	if len(curIssues) > 0 {
		log.Printf("Issues found:\n%s", strings.Join(curIssues, "\n"))
	}
}

func checkCertificateExpiry() (ret issueEntries) {
	client := newHTTPClient()

	issueEntriesChan := make(chan issueEntries, len(conf.CheckCertificateExpiry))

	for _, check := range conf.CheckCertificateExpiry {
		check := check
		issueEntriesChan <- checkCertificateExpiryOf(client, check)
	}

	close(issueEntriesChan)

	for entries := range issueEntriesChan {
		ret = append(ret, entries...)
	}

	return
}

func checkCertificateExpiryOf(client *retryablehttp.Client, url string) (ret issueEntries) {
	log.Printf(" checking certificate expiry on %s", url)

	req, err := retryablehttp.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		ret = append(ret, issueEntry{warning, fmt.Sprintf("%s: invalid certificate check: %s", url, err)})
		return
	}

	// Record per-attempt connection timings so a failure tells us which phase
	// (DNS, connect, TLS, first byte) was slow or hung, from the pod's vantage.
	trace := newRequestTrace()
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace.clientTrace()))
	client.RequestLogHook = func(_ retryablehttp.Logger, _ *http.Request, attempt int) {
		trace.reset(attempt)
	}
	client.CheckRetry = func(ctx context.Context, resp *http.Response, respErr error) (bool, error) {
		shouldRetry, retErr := retryablehttp.DefaultRetryPolicy(ctx, resp, respErr)
		if respErr != nil || shouldRetry {
			logFailedAttempt(http.MethodHead, url, trace, respErr)
		}
		return shouldRetry, retErr
	}

	resp, err := client.Do(req)
	if err != nil {
		ret = append(ret, issueEntry{warning, fmt.Sprintf("%s: error %s", url, err)})
		return
	}
	defer resp.Body.Close()
	if resp.TLS == nil {
		ret = append(ret, issueEntry{warning, fmt.Sprintf("%s: no TLS enabled", url)})
		return
	}

	for _, cert := range resp.TLS.PeerCertificates {
		issuer := strings.Join(cert.Issuer.Organization, ", ")
		daysExpired := int(time.Since(cert.NotAfter).Hours() / 24)
		if daysExpired > 0 {
			ret = append(ret, issueEntry{danger, fmt.Sprintf("%s: certificate from %s has expired %d days", url, issuer, daysExpired)})
		} else if daysExpired > -30 {
			ret = append(ret, issueEntry{warning, fmt.Sprintf("%s: certificate from %s will expire in %d days", url, issuer, -daysExpired)})
		}
	}
	return ret
}

func checkAtumServers() (ret issueEntries) {
	for _, url := range conf.CheckAtumServers {
		ret = append(ret, checkAtumServer(url)...)
	}
	return
}

func checkAtumServer(url string) (ret issueEntries) {
	log.Printf(" checking atum server %s", url)
	ts, err := atum.JsonStamp(url, []byte{1, 2, 3, 4, 5})
	if err != nil {
		ret = append(ret, issueEntry{danger, fmt.Sprintf("%s: requesting Atum stamp failed: %s", url, err)})
		return
	}
	valid, _, url2, err := atum.Verify(ts, []byte{1, 2, 3, 4, 5})
	if err != nil {
		ret = append(ret, issueEntry{danger, fmt.Sprintf("%s: failed to verify signature: %s", url, err)})
		return
	}
	if !valid {
		ret = append(ret, issueEntry{danger, fmt.Sprintf("%s: timestamp invalid", url)})
		return
	}
	if url != url2 {
		ret = append(ret, issueEntry{warning, fmt.Sprintf("%s: timestamp set for wrong url: %s", url, url2)})
		return
	}
	return
}

// The IRMA app keeps functioning when the scheme is down, so all issues that we find are warnings.
func checkSchemeManagers(irmaConfig *irma.Configuration) (ret issueEntries) {
	log.Printf(" checking schememanagers")

	// Clear warnings of previous invocations
	irmaConfig.Warnings = []string{}

	// Schemes are already downloaded in main(), only an update is required now
	// Updating the schemes also automatically reparses them when necessary, populating irmaConfig.Warnings
	err := irmaConfig.UpdateSchemes()
	if err != nil {
		ret = append(ret, issueEntry{warning, fmt.Sprintf("irma scheme verify: update schemes: %s", err)})
		return
	}

	// ParseFolder of UpdateSchemes is skipped when non of the schemes had to be updated. To enforce
	// the warnings from ParseFolder to be generated always, ParseFolder has to be invoked here too.
	// To avoid duplicate warnings, also clear warnings again.
	irmaConfig.Warnings = []string{}
	err = irmaConfig.ParseFolder()
	if err != nil {
		ret = append(ret, issueEntry{warning, fmt.Sprintf("irma scheme verify: parse folder: %s", err)})
		return
	}

	// Check expiry dates on public keys
	if err = irmaConfig.ValidateKeys(); err != nil {
		ret = append(ret, issueEntry{warning, fmt.Sprintf("irma scheme verify: keys: %s", err)})
		return
	}

	for _, warn := range irmaConfig.Warnings {
		ret = append(ret, issueEntry{warning, warn})
	}

	return
}
