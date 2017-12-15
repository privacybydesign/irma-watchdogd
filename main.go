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

	"gopkg.in/yaml.v2"

	"github.com/credentials/irmago"
	schememgr "github.com/credentials/irmago/schememgr/cmd"
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
        <p>Last update at {{ .LastCheck }}</p>
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
	issues         []string
	parsedTemplate *template.Template
)

// Configuration
type Conf struct {
	CheckSchemeManagers map[string]string // {url: pk}
	BindAddr            string            // port to bind to
	Interval            time.Duration
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
		for {
			check()
			<-ticker.C
		}
	}()

	log.Printf("Listening on %s", conf.BindAddr)

	log.Fatal(http.ListenAndServe(conf.BindAddr, nil))
}

// Handle /submit HTTP requests used to submit events
func handler(w http.ResponseWriter, r *http.Request) {
	err := parsedTemplate.Execute(w, templateContext{
		LastCheck: lastCheck.Format("2006-01-02 15:04:05"),
		Issues:    issues,
		Interval:  int(conf.Interval.Seconds() * 1000),
	})
	if err != nil {
		log.Printf("Error executing template: %s", err)
	}
}

func check() {
	newIssues := []string{}

	log.Println("Running checks ...")
	newIssues = append(newIssues, checkSchemeManagers()...)

	issues = newIssues
	lastCheck = time.Now()
	log.Printf("%v", issues)
}

func checkSchemeManagers() []string {
	ret := []string{}
	for url, pk := range conf.CheckSchemeManagers {
		ret = append(ret, checkSchemeManager(url, pk)...)
	}
	return ret
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
