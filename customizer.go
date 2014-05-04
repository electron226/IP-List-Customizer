package customizer

import (
    "appengine"
    "appengine/taskqueue"
    "appengine/urlfetch"
    "bytes"
    "fmt"
    "html"
    "io/ioutil"
    "net/http"
    "net/url"
    "regexp"
    "strconv"
)

var rirList = map[string]string{
    "LACNIC":  "http://ftp.apnic.net/stats/lacnic/delegated-lacnic-extended-latest",
    "ARIN":    "http://ftp.apnic.net/stats/arin/delegated-arin-extended-latest",
    "APNIC":   "http://ftp.apnic.net/stats/apnic/delegated-apnic-extended-latest",
    "AFRINIC": "http://ftp.apnic.net/stats/afrinic/delegated-afrinic-extended-latest",
    "RIPE":    "http://ftp.apnic.net/stats/ripe-ncc/delegated-ripencc-extended-latest",
}
var rirWorkList = map[string][]string{
    "LACNIC":  []string{"ラテンアメリカ", "カリブ海"},
    "ARIN":    []string{"アメリカ"},
    "APNIC":   []string{"アジア", "太平洋"},
    "AFRINIC": []string{"アフリカ"},
    "RIPE":    []string{"ヨーロッパ", "中東", "中央アジア"},
}
var ipCheckRegex = regexp.MustCompile("([a-zA-Z]{2})\\|ipv4\\|(\\d+.\\d+.\\d+.\\d+)\\|(\\d+)")
var ipHashCheckRegex = regexp.MustCompile("[\\d.]+\\|[a-zA-Z]+\\|\\d*\\|\\d*\\|\\d*\\|\\d*\\|[+-]?\\d+")

type IPType map[string]uint
type IPListType map[string][]IPType

func handler(w http.ResponseWriter, r *http.Request) {
    /* context := appengine.NewContext(r) */
    r.ParseForm()
    for key, value := range r.Form {
        fmt.Fprintf(w, "%s : %s\n", key, value)
    }
    fmt.Fprint(w, "Hello, world!")
}

func cronHandler(w http.ResponseWriter, r *http.Request) {
    context := appengine.NewContext(r)
    for rir, u := range rirList {
        task := taskqueue.NewPOSTTask("/update", url.Values{
            "registry": {html.EscapeString(rir)},
            "url":      {html.EscapeString(u)},
        })
        taskqueue.Add(context, task, "")
        context.Infof("Added %s of taskqueue.", rir)
        break
    }
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
    context := appengine.NewContext(r)
    r.ParseForm()
    registry := html.UnescapeString(r.Form["registry"][0])
    update_url := html.UnescapeString(r.Form["url"][0])
    context.Infof("start update of ip list : %s", update_url)

    client := urlfetch.Client(context)
    resp, err := client.Get(update_url)
    if err != nil {
        context.Errorf("I can't get the ip list of registry: %s. code: %s", registry, err.Error())
        return
    }
    defer resp.Body.Close()

    contents, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        context.Errorf("I can't get contents: %s. code: %s", registry, err.Error())
        return
    }

    result := ipCheckRegex.FindAllSubmatch(contents, -1)
    if result != nil {
        iplist := make(IPListType)
        for _, line := range result {
            start, err := getIPtoUint(line[2])
            if err != nil {
                context.Errorf("getIPtoInt is failed. ip = %s, %s", line[2], err)
            }
            buf := bytes.NewBuffer(line[3])
            value, err := strconv.ParseUint(buf.String(), 10, 32)
            if err != nil {
                context.Errorf("updateHandler is failed." +
                    " The error can't convert bytes[] to int.")
            }
            end := start + uint(value)

            ip := IPType{
                "start": start,
                "value": uint(value),
                "end":   end,
            }
            reg := string(line[1])
            if len(iplist[reg]) == 0 {
                iplist[reg] = []IPType{}
            }
            iplist[reg] = append(iplist[reg], ip)
        }

        // To optimize the list of ip.
        context.Infof("optimize start. %s", registry)
        iplist = optimize(iplist)
        context.Infof("optimize end. %s", registry)
    }
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
        v, _ := strconv.ParseUint(buf.String(), 10, 32)
        start += uint(v) << shift_number
        shift_number -= 8
    }

    return start, nil
}

func init() {
    http.HandleFunc("/", handler)
    http.HandleFunc("/cron", cronHandler)
    http.HandleFunc("/update", updateHandler)
}
