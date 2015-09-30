// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// Contributor: Aaron Meihm ameihm@mozilla.com [:alm]

// The MIG loader is a simple bootstrapping tool for MIG. It can be scheduled
// to run on a host system and download the newest available version of the
// agent. If the loader identifies a newer version of the agent available, it
// will download the required files from the API, replace the existing files,
// and notify any existing agent it should terminate.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/jvehent/cljs"
	"io"
	"io/ioutil"
	"mig"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
)

var ctx Context
var haveChanges bool
var apiManifest *mig.ManifestResponse
var wg sync.WaitGroup

func initializeHaveBundle() (ret []mig.BundleDictionaryEntry, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("initializeHaveBundle() -> %v", e)
		}
	}()

	ret, err = mig.GetHostBundle()
	if err != nil {
		panic(err)
	}
	ret, err = mig.HashBundle(ret)
	if err != nil {
		panic(err)
	}
	ctx.Channels.Log <- mig.Log{Desc: "initialized local bundle information"}
	for _, x := range ret {
		hv := x.SHA256
		if hv == "" {
			hv = "not found"
		}
		ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("%v %v -> %v", x.Name, x.Path, hv)}
	}
	return
}

func requestManifest() (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("requestManifest() -> %v", e)
		}
	}()

	murl := APIURL + "manifest"
	ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("requesting manifest from %v", murl)}

	mparam := mig.ManifestParameters{}
	mparam.OS = runtime.GOOS
	mparam.Arch = runtime.GOARCH
	if TAGS.Operator == "" {
		mparam.Operator = "default"
	} else {
		mparam.Operator = TAGS.Operator
	}
	buf, err := json.Marshal(mparam)
	if err != nil {
		panic(err)
	}
	mstring := string(buf)
	data := url.Values{"parameters": {mstring}}
	r, err := http.NewRequest("POST", murl, strings.NewReader(data.Encode()))
	if err != nil {
		panic(err)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := http.Client{}
	resp, err := client.Do(r)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	var resource *cljs.Resource
	err = json.Unmarshal(body, &resource)
	if err != nil {
		panic(err)
	}

	// Extract our manifest from the response.
	manifest, err := valueToManifest(resource.Collection.Items[0].Data[0].Value)
	if err != nil {
		panic(err)
	}
	apiManifest = &manifest
	return
}

func valueToManifest(v interface{}) (m mig.ManifestResponse, err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	err = json.Unmarshal(b, &m)
	return
}

func valueToFetchResponse(v interface{}) (m mig.ManifestFetchResponse, err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	err = json.Unmarshal(b, &m)
	return
}

func fetchFile(n string) (ret []byte, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("fetchFile() -> %v", e)
		}
	}()

	murl := APIURL + "manifest/fetch"

	ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("fetching file from %v", murl)}
	mparam := mig.ManifestParameters{}
	mparam.OS = runtime.GOOS
	mparam.Arch = runtime.GOARCH
	mparam.Operator = TAGS.Operator
	if mparam.Operator == "" {
		mparam.Operator = "default"
	}
	mparam.Object = n
	buf, err := json.Marshal(mparam)
	if err != nil {
		panic(err)
	}
	mstring := string(buf)
	data := url.Values{"parameters": {mstring}}
	r, err := http.NewRequest("POST", murl, strings.NewReader(data.Encode()))
	if err != nil {
		panic(err)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := http.Client{}
	resp, err := client.Do(r)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}

	var resource *cljs.Resource
	err = json.Unmarshal(body, &resource)
	if err != nil {
		panic(err)
	}

	// Extract fetch response.
	fetchresp, err := valueToFetchResponse(resource.Collection.Items[0].Data[0].Value)
	if err != nil {
		panic(err)
	}

	// Decompress the returned file and return it as a byte slice.
	b := bytes.NewBuffer(fetchresp.Data)
	gz, err := gzip.NewReader(b)
	if err != nil {
		panic(err)
	}
	ret, err = ioutil.ReadAll(gz)
	if err != nil {
		panic(err)
	}
	return
}

func fetchAndReplace(entry mig.BundleDictionaryEntry, sig string) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("fetchAndReplace() -> %v", e)
		}
	}()

	// Grab the new file from the API.
	filebuf, err := fetchFile(entry.Name)
	if err != nil {
		panic(err)
	}

	// Stage the new file. Write the file recieved from the API to the
	// file system and validate the signature of the new file to make
	// sure it matches the signature from the manifest.
	//
	// Append .loader to the file name to use as the staged file path.
	reppath := entry.Path + ".loader"
	fd, err := os.OpenFile(reppath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0700)
	if err != nil {
		panic(err)
	}
	_, err = fd.Write(filebuf)
	if err != nil {
		panic(err)
	}
	fd.Close()

	// Validate the signature on the new file.
	ctx.Channels.Log <- mig.Log{Desc: "validating staged file signature"}
	h := sha256.New()
	fd, err = os.Open(reppath)
	if err != nil {
		panic(err)
	}
	buf := make([]byte, 4096)
	for {
		n, err := fd.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			fd.Close()
			panic(err)
		}
		if n > 0 {
			h.Write(buf[:n])
		}
	}
	fd.Close()
	if sig != fmt.Sprintf("%x", h.Sum(nil)) {
		panic("staged file signature mismatch")
	}

	// Got this far, OK to proceed with the replacement.
	ctx.Channels.Log <- mig.Log{Desc: "installing staged file"}
	err = os.Rename(reppath, entry.Path)
	if err != nil {
		panic(err)
	}
	return
}

func checkEntry(entry mig.BundleDictionaryEntry) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("checkEntry() -> %v", e)
		}
	}()

	var compare mig.ManifestEntry
	ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("comparing %v %v", entry.Name, entry.Path)}
	found := false
	for _, x := range apiManifest.Entries {
		if x.Name == entry.Name {
			compare = x
			found = true
			break
		}
	}
	if !found {
		ctx.Channels.Log <- mig.Log{Desc: "entry not in API manifest, ignoring"}
		return
	}
	hv := entry.SHA256
	if hv == "" {
		hv = "not found"
	}
	ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("we have %v", hv)}
	ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("they have %v", compare.SHA256)}
	if entry.SHA256 == compare.SHA256 {
		ctx.Channels.Log <- mig.Log{Desc: "nothing to do here"}
		return
	}
	haveChanges = true
	ctx.Channels.Log <- mig.Log{Desc: fmt.Sprintf("refreshing %v", entry.Name)}
	err = fetchAndReplace(entry, compare.SHA256)
	if err != nil {
		panic(err)
	}
	return
}

// Compare the manifest that the API sent with our knowledge of what is
// currently installed. For each case there is a difference, we will
// request the new file and replace the existing entry.
func compareManifest(have []mig.BundleDictionaryEntry) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("compareManifest() -> %v", e)
		}
	}()

	for _, x := range have {
		err := checkEntry(x)
		if err != nil {
			panic(err)
		}
	}
	return
}

func initContext() (ctx Context, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("initContext() -> %v", e)
		}
	}()

	ctx.Channels.Log = make(chan mig.Log, 37)
	ctx.Logging.Level = "debug"
	ctx.Logging.Mode = "stdout"
	ctx.Logging, err = mig.InitLogger(ctx.Logging, "mig-loader")
	if err != nil {
		panic(err)
	}
	return
}

func doExit(v int) {
	close(ctx.Channels.Log)
	wg.Wait()
	os.Exit(v)
}

func main() {
	var err error
	runtime.GOMAXPROCS(1)

	ctx, err = initContext()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	wg.Add(1)
	go func() {
		var stop bool
		for event := range ctx.Channels.Log {
			stop, err = mig.ProcessLog(ctx.Logging, event)
			if err != nil {
				panic("unable to process log")
			}
			if stop {
				break
			}
		}
		wg.Done()
	}()
	ctx.Channels.Log <- mig.Log{Desc: "logging routine started"}

	// Get our current status from the file system.
	have, err := initializeHaveBundle()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		doExit(1)
	}

	// Retrieve our manifest from the API.
	err = requestManifest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		doExit(1)
	}

	err = compareManifest(have)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		doExit(1)
	}

	if haveChanges {
		err = runTriggers()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			doExit(1)
		}
	}
	doExit(0)
}
