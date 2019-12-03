// (c) 2017 - Bas Westerbaan <bas@westerbaan.name>
// You may redistribute this file under the conditions of the GPLv3.

// irma-watchdogd is a simple webserver that checks various properties of
// the public irma infrastructure.

package main

import (
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	irma "github.com/privacybydesign/irmago"

	"github.com/ashwanthkumar/slack-go-webhook"
	"github.com/dustin/go-humanize"
	"gopkg.in/yaml.v2"

	"github.com/bwesterb/go-atum"
)

var exampleConfig string = `
    bindaddr: ':8079'
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
	lastCheck      time.Time
	initialCheck   bool
	issues         []string
	parsedTemplate *template.Template
)

// Configuration
type Conf struct {
	BindAddr               string // port to bind to
	CheckCertificateExpiry []string
	CheckAtumServers       []string
	Interval               time.Duration
	SlackWebhooks          []string
}

func main() {
	var confPath string

	// set configuration defaults
	conf.BindAddr = ":8079"
	conf.Interval = 5 * time.Minute

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
	}

	buf, err := ioutil.ReadFile(confPath)
	if err != nil {
		log.Fatalf("Could not read config file %s: %s", confPath, err)
	}

	if err := yaml.Unmarshal(buf, &conf); err != nil {
		log.Fatalf("Could not parse config file: %s", err)
	}

	// Load IRMA configuration
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		log.Printf("checkSchemeManager: TempDir: %s", err)
		return
	}
	defer os.RemoveAll(tempDir)

	icDir := path.Join(tempDir, "irma_configuration")
	err = os.Mkdir(icDir, 0700)
	if err != nil {
		log.Printf("MkDir in temp dir for IRMA configuration(%s): %s", icDir, err)
		return
	}
	irmaConfig, err := irma.NewConfiguration(icDir)
	if err != nil {
		log.Printf("IRMA configuration could not be loaded in temp dir %s", icDir)
		return
	}
	err = irmaConfig.DownloadDefaultSchemes()
	if err != nil {
		log.Printf("default IRMA configuration cannot be downloaded")
		return
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
		initialCheck = true
		for {
			check(irmaConfig)
			<-ticker.C
		}
	}()

	log.Printf("Listening on %s", conf.BindAddr)

	log.Fatal(http.ListenAndServe(conf.BindAddr, nil))
}

// Handle / HTTP request
func handler(w http.ResponseWriter, r *http.Request) {
	err := parsedTemplate.Execute(w, templateContext{
		LastCheck: humanize.Time(lastCheck),
		Issues:    issues,
		Interval:  int(conf.Interval.Seconds() * 1000),
	})
	if err != nil {
		log.Printf("Error executing template: %s", err)
	}
}

// Computes difference between old and new issues
func difference(old, cur []string) (came, gone []string) {
	came = []string{}
	gone = []string{}
	lut := make(map[string]bool)
	for _, x := range old {
		lut[x] = true
	}
	for _, x := range cur {
		if _, ok := lut[x]; !ok {
			came = append(came, x)
		} else {
			lut[x] = false
		}
	}
	for x, isGone := range lut {
		if isGone {
			gone = append(gone, x)
		}
	}
	return
}

func check(irmaConfig *irma.Configuration) {
	curIssues := []string{}

	log.Println("Running checks ...")
	curIssues = append(curIssues, checkSchemeManagers(irmaConfig)...)
	curIssues = append(curIssues, checkCertificateExpiry()...)
	curIssues = append(curIssues, checkAtumServers()...)

	if len(conf.SlackWebhooks) > 0 {
		newIssues, fixedIssues := difference(issues, curIssues)
		go pushToSlack(newIssues, fixedIssues, initialCheck)
	}

	issues = curIssues
	initialCheck = false
	lastCheck = time.Now()
}

func pushToSlack(newIssues, fixedIssues []string, initial bool) {
	strGood := "good"
	strBad := "bad"
	for _, url := range conf.SlackWebhooks {
		if len(newIssues) > 0 {
			text := "New issues discovered."
			if initial {
				text = "I just (re)started and found the following issues."
			}
			payload := slack.Payload{
				Text:        text,
				Username:    "irma-watchdogd",
				IconEmoji:   ":dog:",
				Attachments: []slack.Attachment{},
			}
			for _, newIssue := range newIssues {
				newIssue := newIssue
				payload.Attachments = append(payload.Attachments, slack.Attachment{
					Fallback: &newIssue,
					Text:     &newIssue,
					Color:    &strBad,
				})
			}
			if err := slack.Send(url, "", payload); err != nil {
				log.Printf("SlackWebhook %s: %s", url, err)
				continue
			}
		}
		if len(fixedIssues) > 0 {
			payload := slack.Payload{
				Text:        "The following issues were fixed.",
				Username:    "irma-watchdogd",
				IconEmoji:   ":dog:",
				Attachments: []slack.Attachment{},
			}
			for _, fixedIssue := range fixedIssues {
				fixedIssue := fixedIssue
				payload.Attachments = append(payload.Attachments, slack.Attachment{
					Fallback: &fixedIssue,
					Text:     &fixedIssue,
					Color:    &strGood,
				})
			}
			if err := slack.Send(url, "", payload); err != nil {
				log.Printf("SlackWebhook %s: %s", url, err)
				continue
			}
		}
	}
}

func logCurrentIssues(curIssues []string) {
	if len(curIssues) > 0 {
		log.Printf("Issues found:\n%s", strings.Join(curIssues, "\n"))
	}
}

func checkCertificateExpiry() []string {
	ret := []string{}
	for _, url := range conf.CheckCertificateExpiry {
		log.Printf(" checking certificate expiry on %s", url)
		ret = append(ret, checkCertificateExpiryOf(url)...)
	}
	logCurrentIssues(ret)
	return ret
}

func checkCertificateExpiryOf(url string) (ret []string) {
	ret = []string{}
	resp, err := http.Head(url)
	if err != nil {
		ret = append(ret, fmt.Sprintf("%s: error %s", url, err))
		return
	}
	defer resp.Body.Close()
	if resp.TLS == nil {
		ret = append(ret, fmt.Sprintf("%s: no TLS enabled", url))
		return
	}

	for _, cert := range resp.TLS.PeerCertificates {
		issuer := strings.Join(cert.Issuer.Organization, ", ")
		daysExpired := int(time.Since(cert.NotAfter).Hours() / 24)
		if daysExpired > 0 {
			ret = append(ret, fmt.Sprintf("%s: certificate from %s has expired %d days", url, issuer, daysExpired))
		} else if daysExpired > -30 {
			ret = append(ret, fmt.Sprintf("%s: certificate from %s will expire in %d days", url, issuer, -daysExpired))
		}
	}
	return ret
}

func checkAtumServers() []string {
	ret := []string{}
	for _, url := range conf.CheckAtumServers {
		ret = append(ret, checkAtumServer(url)...)
	}
	logCurrentIssues(ret)
	return ret
}

func checkAtumServer(url string) (ret []string) {
	ret = []string{}
	log.Printf(" checking atum server %s", url)
	ts, err := atum.JsonStamp(url, []byte{1, 2, 3, 4, 5})
	if err != nil {
		ret = append(ret, fmt.Sprintf("%s: requesting Atum stamp failed: %s", url, err))
		return
	}
	valid, _, url2, err := atum.Verify(ts, []byte{1, 2, 3, 4, 5})
	if err != nil {
		ret = append(ret, fmt.Sprintf("%s: failed to verify signature: %s", url, err))
		return
	}
	if !valid {
		ret = append(ret, fmt.Sprintf("%s: timestamp invalid", url))
		return
	}
	if url != url2 {
		ret = append(ret, fmt.Sprintf("%s: timestamp set for wrong url: %s", url, url2))
		return
	}
	return
}

func checkSchemeManagers(irmaConfig *irma.Configuration) (ret []string) {
	ret = []string{}
	log.Printf(" checking schememanagers")

	// Clear warnings of previous invocations
	irmaConfig.Warnings = []string{}

	// Schemes are already downloaded in main(), only an update is required now
	// Updating the schemes also automatically reparses them when necessary, populating irmaConfig.Warnings
	err := irmaConfig.UpdateSchemes()
	if err != nil {
		ret = append(ret, fmt.Sprintf("irma scheme verify: update schemes: %s", err))
		return
	}

	// ParseFolder of UpdateSchemes is skipped when non of the schemes had to be updated. To enforce
	// the warnings from ParseFolder to be generated always, ParseFolder has to be invoked here too.
	// To avoid duplicate warnings, also clear warnings again.
	irmaConfig.Warnings = []string{}
	err = irmaConfig.ParseFolder()
	if err != nil {
		ret = append(ret, fmt.Sprintf("irma scheme verify: parse folder: %s", err))
		return
	}

	// Check expiry dates on public keys
	if err = irmaConfig.ValidateKeys(); err != nil {
		ret = append(ret, fmt.Sprintf("irma scheme verify: keys: %s", err))
		return
	}

	for _, manager := range irmaConfig.SchemeManagers {
		// Verify signatures
		if err = irmaConfig.VerifySchemeManager(manager); err != nil {
			ret = append(ret, fmt.Sprintf("irma scheme verify: signature verification %s: %s", manager.ID, err))
		}
	}

	ret = append(ret, irmaConfig.Warnings...)
	logCurrentIssues(ret)
	return
}
