package main

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"time"

	"appengine"
	"appengine/datastore"
	"appengine/memcache"
	"appengine/taskqueue"
	"appengine/urlfetch"
)

var rirList = map[string]string{
	"LACNIC":  "http://ftp.apnic.net/stats/lacnic/delegated-lacnic-extended-latest",
	"ARIN":    "http://ftp.apnic.net/stats/arin/delegated-arin-extended-latest",
	"APNIC":   "http://ftp.apnic.net/stats/apnic/delegated-apnic-extended-latest",
	"AFRINIC": "http://ftp.apnic.net/stats/afrinic/delegated-afrinic-extended-latest",
	"RIPE":    "http://ftp.apnic.net/stats/ripe-ncc/delegated-ripencc-extended-latest",
}
var ipCheckRegex = regexp.MustCompile("([a-zA-Z]{2})\\|ipv4\\|(\\d+.\\d+.\\d+.\\d+)\\|(\\d+)")
var ipHeaderCheckRegex = regexp.MustCompile("[\\d.]+\\|[a-zA-Z]+\\|(\\d*)\\|\\d*\\|\\d*\\|\\d*\\|[+-]?\\d+")
var dateCheckRegex = regexp.MustCompile("^(\\d{4})(\\d{2})(\\d{2})$")
var replaceCheckRegex = regexp.MustCompile("{[A-Z]+}")

const DATE_KEY = "LATEST_DATE"
const ETAG_KEY = "LATEST_ETAG"
const MEMCACHE_TEMPLATE = "TEMPLATE"
const MEMCACHE_LIST = "LIST"

type IPType map[string]uint
type IPListType map[string][]IPType

type Store struct {
	Data []byte
}

type TemplateArguments struct {
	Date       string
	Countries  map[string]int
	Registries sort.StringSlice
}

type WriterForMim struct {
	io.Writer

	Data bytes.Buffer
}

func (p *WriterForMim) Write(b []byte) (n int, err error) {
	p.Data.Write(b)
	return p.Data.Len(), nil
}

func (p *WriterForMim) GetBytes() []byte {
	return p.Data.Bytes()
}

func getKeysOnDS(c appengine.Context, kind string) ([]*datastore.Key, []Store, error) {
	var u []Store
	query := datastore.NewQuery(kind)
	keys, err := query.GetAll(c, &u)
	if err != nil {
		_, file, errorLine, _ := runtime.Caller(0)
		return nil, nil, fmt.Errorf("I can't query %s.\nmessage: %s\nfile: %s\nline: %d",
			kind, err.Error(), file, errorLine)
	}
	return keys, u, err
}

func handler(w http.ResponseWriter, r *http.Request) {
	context := appengine.NewContext(r)

	// If already exist the recent date in MemCache, this program use it.
	if tCache, err := memcache.Get(context, MEMCACHE_TEMPLATE); err == nil {
		fmt.Fprintf(w, "%s", tCache.Value)
	} else {
		// Get the catalogue of the registries.
		keys, _, err := getKeysOnDS(context, DATE_KEY)
		if err != nil {
			fmt.Fprintf(w, err.Error())
		}

		arguments := new(TemplateArguments)
		arguments.Countries = make(map[string]int)

		// To get the list of registries.
		for _, v := range keys {
			arguments.Registries = append(arguments.Registries, v.StringID())
		}
		arguments.Registries.Sort()

		// To get the latest date of the list.
		u := make([]Store, len(keys))
		err = datastore.GetMulti(context, keys, u)
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			fmt.Fprintf(w,
				"I can't get latest dates.\nmessage: %s\nfile: %s\nline: %d",
				err.Error(), file, errorLine)
		}

		var latest_date time.Time
		for _, v := range u {
			r := dateCheckRegex.FindSubmatch(v.Data)
			if r != nil {
				year, _ := strconv.Atoi(string(r[1]))
				month, _ := strconv.Atoi(string(r[2]))
				day, _ := strconv.Atoi(string(r[3]))
				if err != nil {
					_, file, errorLine, _ := runtime.Caller(0)
					fmt.Fprintf(w,
						"I don't get latest date.\nfile: %s\nline: %d", file, errorLine)
					continue
				}
				t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
				if latest_date.IsZero() || t.After(latest_date) {
					latest_date = t
				}
			}
		}
		if latest_date.IsZero() {
			arguments.Date = "Don't update."
		} else {
			arguments.Date = fmt.Sprintf(
				"%d/%02d/%02d", latest_date.Year(), latest_date.Month(), latest_date.Day())
		}

		// Get the catalogue of the countries.
		for _, registry := range arguments.Registries {
			var u []Store
			query := datastore.NewQuery(registry)
			keys, err := query.GetAll(context, &u)
			if err != nil {
				_, file, errorLine, _ := runtime.Caller(0)
				fmt.Fprintf(w, "I can't query %s.\nmessage: %s\nfile: %s\nline: %d",
					registry, err.Error(), file, errorLine)
			}
			for _, v := range keys {
				arguments.Countries[v.StringID()]++
			}
		}

		/* a template convert to html, then show it. */
		wfm := new(WriterForMim)
		t := template.Must(template.ParseFiles("index.html"))
		t.Execute(wfm, arguments)

		// be adding the cache of the template to MemCache.
		item := &memcache.Item{
			Key:   MEMCACHE_TEMPLATE,
			Value: wfm.GetBytes(),
		}
		if err = memcache.Set(context, item); err != nil {
			context.Warningf("Don't set the template cache to MemCache. message: %s", err.Error())
		}

		fmt.Fprintf(w, "%s", wfm.GetBytes())
	}
}

func createAllCacheOnDS(context appengine.Context) (map[string]IPListType, error) {
	// Get the catalogue of the registries.
	keys, _, err := getKeysOnDS(context, DATE_KEY)
	if err != nil {
		return nil, fmt.Errorf("%v", err.Error())
	}

	// To get the list of registries.
	var regs sort.StringSlice
	for _, v := range keys {
		regs = append(regs, v.StringID())
	}
	regs.Sort()

	var ips []IPType
	cache := make(map[string]IPListType)
	for _, r := range regs {
		keys, u, err := getKeysOnDS(context, r)
		if err != nil {
			return nil, fmt.Errorf("%v", err.Error())
		}

		cache[r] = make(IPListType)
		for i, v := range u {
			err = json.Unmarshal(v.Data, &ips)
			if err != nil {
				_, file, errorLine, _ := runtime.Caller(0)
				return nil, fmt.Errorf(
					"I can't get the json data.\nmessage: %s\nfile: %s\nline: %d",
					err.Error(), file, errorLine)
			}

			if cache[r][keys[i].StringID()] != nil {
				_, file, errorLine, _ := runtime.Caller(0)
				return nil, fmt.Errorf(
					"The cache data already have gotten.\nfile: %s\nline: %d",
					file, errorLine)
			}
			cache[r][keys[i].StringID()] = ips
		}
	}
	return cache, nil
}

func getHandler(w http.ResponseWriter, r *http.Request) {
	context := appengine.NewContext(r)

	r.ParseForm()

	var listCache = make(map[string]IPListType)
	if tCache, err := memcache.Get(context, MEMCACHE_LIST); err == nil {
		// decompression by zlib.
		var buffer bytes.Buffer
		reader, err := zlib.NewReader(bytes.NewBuffer(tCache.Value))
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			fmt.Fprintf(w,
				"Couldn't craete new reader of zlib.\nmessage: %s\nfile: %s\nline: %d",
				err.Error(), file, errorLine)
			return
		}
		_, err = io.Copy(&buffer, reader)
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			fmt.Fprintf(w,
				"Couldn't copy from reader to buffer.\nmessage: %s\nfile: %s\nline: %d",
				err.Error(), file, errorLine)
			return
		}
		err = reader.Close()
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			fmt.Fprintf(w,
				"Reader wasn't closed.\nmessage: %s\nfile: %s\nline: %d",
				err.Error(), file, errorLine)
			return
		}

		// convert the json data to the cache data.
		err = json.Unmarshal(buffer.Bytes(), &listCache)
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			fmt.Fprintf(w,
				"Couldn't conver from json data to cache data.\nmessage: %s\nfile: %s\nline: %d",
				err.Error(), file, errorLine)
			return
		}
	} else {
		cache, err := createAllCacheOnDS(context)
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			fmt.Fprintf(w,
				"Coundn't create all cache data.\nmessage: %s\nfile: %s\nline: %d",
				err.Error(), file, errorLine)
			return
		}
		listCache = cache

		// convert the cache data to json data.
		values, err := json.Marshal(cache)
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			fmt.Fprintf(w,
				"Coundn't convert from cache data to json data.\nmessage: %s\nfile: %s\nline: %d",
				err.Error(), file, errorLine)
			return
		}

		// compession by zlib.
		var buffer bytes.Buffer
		writer := zlib.NewWriter(&buffer)
		_, err = writer.Write(values)
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			fmt.Fprintf(w,
				"Coundn't compression by zlib.\nmessage: %s\nfile: %s\nline: %d",
				err.Error(), file, errorLine)
			return
		}
		err = writer.Close()
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			fmt.Fprintf(w,
				"Coundn't close new writer of zlib.\nmessage: %s\nfile: %s\nline: %d",
				err.Error(), file, errorLine)
			return
		}

		// set memcache.
		item := &memcache.Item{
			Key:   MEMCACHE_LIST,
			Value: buffer.Bytes(),
		}
		if err = memcache.Set(context, item); err != nil {
			context.Warningf("Don't set the template cache to MemCache. message: %s", err.Error())
		}
	}

	outputList := make(map[string]map[string]bool)
	for reg, v := range listCache {
		t := make(map[string]bool)
		if r.Form[reg] != nil {
			for c, _ := range v {
				t[c] = true
			}
		} else {
			for c, _ := range v {
				if r.Form[c] != nil {
					t[c] = true
				}
			}
		}
		if len(t) > 0 {
			outputList[reg] = t
		}
	}

	customString := r.Form["custom"][0]
	for reg, v := range outputList {
		for cc, _ := range v {
			for _, ip := range listCache[reg][cc] {
				text := replaceCheckRegex.ReplaceAllStringFunc(
					customString,
					func(m string) string {
						switch m {
						case "{REG}":
							return reg
						case "{CC}":
							return cc
						case "{START}":
							return string(getUintToIP(ip["start"]))
						case "{END}":
							return string(getUintToIP(ip["end"]))
						}
						return m
					},
				)
				fmt.Fprintln(w, text)
			}
		}
	}
}

func cronHandler(w http.ResponseWriter, r *http.Request) {
	context := appengine.NewContext(r)
	var taskList []*taskqueue.Task
	for rir, u := range rirList {
		task := taskqueue.NewPOSTTask("/update", url.Values{
			"registry": {html.EscapeString(rir)},
			"url":      {html.EscapeString(u)},
		})
		task.Header.Set("Host", appengine.BackendHostname(context, "updatebackend", 1))
		taskList = append(taskList, task)
		context.Infof("Added %s of taskqueue.", rir)
	}
	taskqueue.AddMulti(context, taskList, "update")
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	context := appengine.Timeout(appengine.NewContext(r), 30*time.Second)

	r.ParseForm()
	registry := html.UnescapeString(r.Form["registry"][0])
	update_url := html.UnescapeString(r.Form["url"][0])
	context.Infof("update of ip list is starting: %s", update_url)

	client := &http.Client{
		Transport: &urlfetch.Transport{
			Context:  context,
			Deadline: 60 * time.Second,
		},
	}

	// The program gets Etag in the datastore. And it's set in the header. Then send request it.
	// If returned StatusCode is 304, already the list is the latest version.
	// Else case be updating the list.
	var eTag []byte
	tempData := new(Store)
	key := datastore.NewKey(context, ETAG_KEY, registry, 0, nil)
	if err := datastore.Get(context, key, tempData); err == nil {
		eTag = tempData.Data
	} else if err == datastore.ErrNoSuchEntity {
		context.Infof("%s in %s wasn't found. so I add the new date.", registry, ETAG_KEY)
	} else {
		_, file, errorLine, _ := runtime.Caller(0)
		context.Criticalf("I can't get %s : %s.\nmessage: %s\n"+
			"file: %s\nline: %d", ETAG_KEY, registry, err.Error(), file, errorLine)
		return
	}

	if len(eTag) > 0 {
		context.Infof("Compare new eTag and old Etag.")

		req, err := http.NewRequest("HEAD", update_url, nil)
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			context.Criticalf("I can't get the header of the iplist of registry: %s.\n"+
				"message: %s\nfile: %s\nline: %d", registry, err.Error(),
				file, errorLine)
			return
		}

		req.Header.Add("If-None-Match", fmt.Sprintf("%s", eTag))

		resp, err := client.Do(req)
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			context.Criticalf("I can't doing Do function of the header of the iplist of registry: %s.\n"+
				"message: %s\nstatus: %s\nfile: %s\nline: %d", registry, err.Error(),
				resp.Status, file, errorLine)
			return
		}

		if resp.StatusCode == http.StatusNotModified {
			context.Infof("the ip list of %s wasn't updated. so this process is closing.", registry)
			return
		}
	}

	context.Infof("get the list of %s.", registry)

	resp, err := client.Get(update_url)
	defer resp.Body.Close()
	if err != nil {
		_, file, errorLine, _ := runtime.Caller(0)
		context.Criticalf("I can't get the ip list of registry: %s.\n"+
			"message: %s\nstatus: %d\nfile: %s\nline: %d", registry, err.Error(),
			resp.Status, file, errorLine)
		return
	}

	var newEtag []byte
	tempEtag := resp.Header["Etag"] // If etag isn't exist, the variable is nil.
	if tempEtag != nil {
		// eTag[0] is take out Etag code in array of eTag.
		eTagBuf := bytes.NewBufferString(tempEtag[0])
		newEtag = eTagBuf.Bytes()
	}

	contents, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		_, file, errorLine, _ := runtime.Caller(0)
		context.Criticalf("I can't get contents: %s.\nmessage: %s"+
			"\nfile: %s\nline: %d", registry, err.Error(), file, errorLine)
		return
	}

	// get the date of latest update.
	date := ipHeaderCheckRegex.FindSubmatch(contents)
	if date == nil {
		_, file, errorLine, _ := runtime.Caller(0)
		context.Criticalf("I can't get consistent contents "+
			"to ipHeaderCheckRegex.\nmessage: %s\nfile: %s\nline: %d",
			err.Error(), file, errorLine)
		return
	}

	context.Infof("The list of %s is starting update.", registry)

	result := ipCheckRegex.FindAllSubmatch(contents, -1)
	if result == nil {
		_, file, errorLine, _ := runtime.Caller(0)
		context.Criticalf("I can't get consistent contents to ipCheckRegex.\n"+
			"message: %s\nfile: %s\nline: %d", err.Error(), file, errorLine)
		return
	}
	iplist := make(IPListType)
	for _, line := range result {
		start, err := getIPtoUint(line[2])
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			context.Errorf("getIPtoInt is failed. ip = %s,\nmessage: %s\n"+
				"file: %s\nline: %d", line[2], err.Error(), file, errorLine)
			continue
		}
		buf := bytes.NewBuffer(line[3])
		value, err := strconv.Atoi(buf.String())
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			context.Errorf("updateHandler is failed.\n"+
				" The error can't convert bytes[] to int.\nmessage: %s\n"+
				"file: %s\nline: %d", err.Error(), file, errorLine)
			continue
		}
		end := start + uint(value)

		ip := IPType{
			"start": start,
			"value": uint(value),
			"end":   end,
		}
		country := string(line[1])
		if len(iplist[country]) == 0 {
			iplist[country] = []IPType{}
		}
		iplist[country] = append(iplist[country], ip)
	}

	// To optimize the list of ip.
	context.Infof("optimize start. %s", registry)
	iplist = optimize(iplist)
	context.Infof("optimize end. %s", registry)

	context.Infof("Write the ip list and Etag.")

	err = datastore.RunInTransaction(context, func(c appengine.Context) error {
		// To delete old registry on datastore.
		keys, _, err := getKeysOnDS(context, registry)
		if err != nil {
			context.Errorf(err.Error())
			return err
		}
		err = datastore.DeleteMulti(context, keys)
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			context.Errorf("I can't delete %s.\nmessage: %s\n"+
				"file: %s\nline: %d", registry, err.Error(), file, errorLine)
			return err
		}

		// To add the countries of the registry on datastore.
		for country, list := range iplist {
			jd, err := json.Marshal(list)
			if err != nil {
				_, file, errorLine, _ := runtime.Caller(0)
				context.Errorf("the list isn't converted.\nmessage: %s\n"+
					"file: %s\nline: %d", err.Error(), file, errorLine)
				return err
			}

			entry := Store{
				Data: jd,
			}
			key, err := datastore.Put(context, datastore.NewKey(context, registry, country, 0, nil), &entry)
			if err != nil {
				_, file, errorLine, _ := runtime.Caller(0)
				context.Errorf("the list of %s is not put.\nmessage: %s\n"+
					"file: %s\nline: %d", key, err.Error(), file, errorLine)
				return err
			}
		}

		// To write the new date of a registry.
		key := datastore.NewKey(context, DATE_KEY, registry, 0, nil)
		err = datastore.Delete(context, key)
		if !(err == nil || err == datastore.ErrNoSuchEntity) {
			_, file, errorLine, _ := runtime.Caller(0)
			context.Criticalf("I can't delete %s.\nmessage: %s\n"+
				"file: %s\nline: %d", ETAG_KEY, err.Error(), file, errorLine)
			return err
		}

		entry := Store{
			Data: date[1],
		}
		key, err = datastore.Put(context, key, &entry)
		if err != nil {
			_, file, errorLine, _ := runtime.Caller(0)
			context.Criticalf("the list of %s wasn't wrote date.\nmessage: %s\n"+
				"file: %s\nline: %d", key, err.Error(), file, errorLine)
			return err
		}

		// To write the new ETag of a registry.
		if len(newEtag) > 0 {
			key := datastore.NewKey(context, ETAG_KEY, registry, 0, nil)
			err := datastore.Delete(context, key)
			if !(err == nil || err == datastore.ErrNoSuchEntity) {
				_, file, errorLine, _ := runtime.Caller(0)
				context.Criticalf("I can't delete %s.\nmessage: %s\n"+
					"file: %s\nline: %d", ETAG_KEY, err.Error(), file, errorLine)
				return err
			}

			entry := Store{
				Data: newEtag,
			}
			key, err = datastore.Put(context, key, &entry)
			if err != nil {
				_, file, errorLine, _ := runtime.Caller(0)
				context.Criticalf("the list of %s wasn't wrote date.\nmessage: %s\n"+
					"file: %s\nline: %d", key, err.Error(), file, errorLine)
				return err
			}
		} else {
			context.Infof("the program isn't write the eTag into datastore because it don't get eTag.")
		}
		return err
	}, nil)
	if err != nil {
		context.Errorf("the transaction of update process was failed: %v", err)
		return
	}

	context.Infof("Clear the caches in MemCache.")

	err = memcache.DeleteMulti(context, []string{MEMCACHE_TEMPLATE, MEMCACHE_LIST})
	if !(err == nil || err == memcache.ErrCacheMiss) {
		context.Errorf("occur the error when memcache is deleted.: %v", err)
	}

	context.Infof("update of ip list is end: %s", update_url)
}

func concat(left, right []IPType) []IPType {
	slice := make([]IPType, len(left)+len(right))
	copy(slice, left)
	copy(slice[len(left):], right)
	return slice
}

func optimize(list IPListType) IPListType {
	for r, l := range list {
		i := 0
		j := i + 1
		for j < len(l) {
			if l[i]["end"] == l[j]["start"] {
				l[i]["end"] = l[j]["end"]
				l[i]["value"] += l[j]["value"]
				j++
			} else {
				if j-i+1 > 0 {
					l = concat(l[:i+1], l[j:])
				}
				i++
				j = i + 1
			}
		}
		if j-i+1 > 0 {
			l = concat(l[:i+1], l[j:])
		}
		list[r] = l
	}
	return list
}

func getUintToIP(value uint) []byte {
	return []byte(fmt.Sprintf("%d.%d.%d.%d",
		(value&0xFF000000)>>24, (value&0x00FF0000)>>16,
		(value&0x0000FF00)>>8, (value & 0x000000FF)))
}

func getIPtoUint(ip []byte) (uint, error) {
	ips := bytes.Split(ip, []byte("."))
	if len(ips) != 4 {
		return 0, fmt.Errorf("GetIPValues is failed." +
			" The error is that the length of the first argument isn't 4.")
	}

	var start uint = 0
	var shift_number uint = 24
	for _, i := range ips {
		buf := bytes.NewBuffer(i)
		v, _ := strconv.Atoi(buf.String())
		start += uint(v) << shift_number
		shift_number -= 8
	}

	return start, nil
}

func init() {
	http.HandleFunc("/", handler)
	http.HandleFunc("/cron", cronHandler)
	http.HandleFunc("/update", updateHandler)
	http.HandleFunc("/get", getHandler)
}
