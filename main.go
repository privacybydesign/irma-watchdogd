// (c) 2017 - Bas Westerbaan <bas@westerbaan.name>
// You may redistribute this file under the conditions of the GPLv3.

// irma-watchdogd is a simple webserver that checks various properties of
// the public irma infrastructure.

package main

import (
	"encoding/xml"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	netUrl "net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ashwanthkumar/slack-go-webhook"
	"github.com/dustin/go-humanize"
	"gopkg.in/yaml.v2"

	"github.com/bwesterb/go-atum"
	"github.com/privacybydesign/irmago"
	schememgr "github.com/privacybydesign/irmago/schememgr/cmd"
)

var exampleConfig string = `
    checkschememanagers:
        https://privacybydesign.foundation/schememanager/pbdf:
            |
                -----BEGIN PUBLIC KEY-----
                MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAELzHV5ipBimWpuZIDaQQd+KmNpNop
                dpBeCqpDwf+Grrw9ReODb6nwlsPJ/c/gqLnc+Y3sKOAJ2bFGI+jHBSsglg==
                -----END PUBLIC KEY-----
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
	CheckSchemeManagers    map[string]string // {url: pk}
	BindAddr               string            // port to bind to
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
			check()
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

func check() {
	curIssues := []string{}

	log.Println("Running checks ...")
	curIssues = append(curIssues, checkSchemeManagers()...)
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

func checkCertificateExpiry() []string {
	ret := []string{}
	for _, url := range conf.CheckCertificateExpiry {
		ret = append(ret, checkCertificateExpiryOf(url)...)
	}
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
	return ret
}

func checkSchemeManagers() []string {
	ret := []string{}
	for url, pk := range conf.CheckSchemeManagers {
		ret = append(ret, checkSchemeManager(url, pk)...)
	}
	return ret
}

func checkAtumServer(url string) (ret []string) {
	ret = []string{}
	log.Printf(" checking atum sever %s", url)
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

func checkSchemeManager(url, pk string) (ret []string) {
	ret = []string{}
	log.Printf(" checking schememanager %s", url)

	// First, we download all the files of the schememanager.
	// We need a temporary directory to store the files.
	// As `schememgr verify' is a bit picky, we put everything in
	//    <temp dir>/irma_configuration/<name of schememgr>
	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		log.Printf("checkSchemeManager: TempDir: %s", err)
		return
	}
	defer os.RemoveAll(tempDir)

	icDir := path.Join(tempDir, "irma_configuration")
	err = os.Mkdir(icDir, 0700)
	if err != nil {
		log.Printf("checkSchemeManager: MkDir(%s): %s", icDir, err)
		return
	}

	parsedUrl, err := netUrl.Parse(url)
	if err != nil {
		ret = append(ret, fmt.Sprintf("Failed to parse url %s: %s", url, err))
		return
	}
	_, name := path.Split(parsedUrl.Path)

	baseDir := path.Join(icDir, name)
	err = os.Mkdir(baseDir, 0700)
	if err != nil {
		log.Printf("checkSchemeManager: MkDir(%s): %s", icDir, err)
		return
	}

	// Helper.
	download := func(fn string) error {
		resp, err := http.Get(url + "/" + fn)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("HTTP Code %d", resp.StatusCode)
		}
		defer resp.Body.Close()

		localFn := path.Join(baseDir, fn)
		localDir := filepath.Dir(localFn)
		if _, err := os.Stat(localDir); os.IsNotExist(err) {
			if err = os.MkdirAll(localDir, 0700); err != nil {
				return fmt.Errorf("local: os.MkdirAll(%s): %s", localDir, err)
			}
		}

		fh, err := os.Create(localFn)
		if err != nil {
			return fmt.Errorf("local: os.Create(%s): %s", localFn, err)
		}
		defer fh.Close()

		_, err = io.Copy(fh, resp.Body)
		if err != nil {
			return fmt.Errorf("while downloading %s/%s: %s", url, fn, err)
		}
		return nil
	}

	// Download index.sig and index separately
	if err = download("index.sig"); err != nil {
		ret = append(ret, fmt.Sprintf("%s: failed to download index signature: %s", url, err))
	}
	if err = download("pk.pem"); err != nil {
		ret = append(ret, fmt.Sprintf("%s: failed to download pk.pem: %s", url, err))
	}
	if err = download("index"); err != nil {
		ret = append(ret, fmt.Sprintf("%s: failed to download index: %s", url, err))
		return
	}

	// Overwrite public key
	if err = ioutil.WriteFile(path.Join(baseDir, "pk.pem"), []byte(pk), 0600); err != nil {
		log.Printf("checkSchemeManager: failed to write pk.pem: %s", err)
		return
	}

	// Parse index
	var idx irma.SchemeManagerIndex = make(map[string]irma.ConfigurationFileHash)
	idxBytes, err := ioutil.ReadFile(path.Join(baseDir, "index"))
	if err != nil {
		log.Printf("checkSchemeManager: failed to open downloaded index file: %s", err)
		return
	}
	err = idx.FromString(string(idxBytes))
	if err != nil {
		ret = append(ret, fmt.Sprintf("%s: failed to parse index: %s", url, err))
		return
	}

	// Download files
	ok := true
	for fn, _ := range idx {
		bits := strings.SplitN(fn, "/", 2)
		if len(bits) != 2 {
			ret = append(ret, fmt.Sprintf("%s: unexpected index entry: %v", url, fn))
			continue
		}
		err = download(bits[1])
		if err != nil {
			ok = false
			ret = append(ret, fmt.Sprintf("%s: failed to download %s: %s", url, bits[1], err))
		}
	}
	if !ok {
		return
	}

	// Verify signatures
	err = schememgr.RunVerify(icDir)
	if err != nil {
		ret = append(ret, fmt.Sprintf("%s: schememgr verify: %s", url, err))
	}

	// Check expiry dates on public keys
	pkDirs, err := filepath.Glob(path.Join(icDir, "*/*/PublicKeys"))
	if err != nil {
		log.Printf("checkSchemeManager: Glob(*/*/PublicKeys): %s", err)
		return
	}

	for _, pkDir := range pkDirs {
		pks, err := filepath.Glob(path.Join(pkDir, "*.xml"))
		if err != nil {
			log.Printf("checkSchemeManager: Glob: %s", err)
			return
		}
		var maxExpiry int64 = 0
		for _, pk := range pks {
			var pkData struct{ ExpiryDate int64 }
			pkBytes, err := ioutil.ReadFile(pk)
			if err != nil {
				log.Printf("checkSchemeManager: ReadFile(%s): %s", pk, err)
				return
			}
			if err = xml.Unmarshal(pkBytes, &pkData); err != nil {
				ret = append(ret, fmt.Sprintf("%s: failed to parse %s: %s", url, pk, err))
				return
			}
			if maxExpiry < pkData.ExpiryDate {
				maxExpiry = pkData.ExpiryDate
			}
		}
		daysExpired := time.Since(time.Unix(maxExpiry, 0)).Hours() / 24
		pkDirRel, _ := filepath.Rel(icDir, pkDir)
		pkDirBits := strings.Split(pkDirRel, "/")
		if daysExpired > 0 {
			ret = append(ret, fmt.Sprintf("%s: publickey for %s.%s has expired %d days",
				url, pkDirBits[0], pkDirBits[1], int(daysExpired)))
		} else if daysExpired > -30 {
			ret = append(ret, fmt.Sprintf("%s: publickey for %s.%s will expire in %d days",
				url, pkDirBits[0], pkDirBits[1], int(-daysExpired)))
		}
	}

	return
}
