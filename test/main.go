package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
)

func main() {
	factory := SimpleConnFactory("tcp", "194.213.102.182:9000")
	client := SimpleClientFactory(factory, 1000)

	rec := httptest.NewRecorder()

	data := []byte(`{"action": "set", "account":"16399", "ip": "100.127.6.42/32", "bandwidth": 50, "switch_ip": "10.42.0.6", "service_link_id": "1"}`)
	var js map[string]interface{}
	if err := json.Unmarshal(data, &js); err != nil {
		panic(err)
	}

	vals := make(url.Values)

	for k, v := range js {
		var val string
		switch t := v.(type) {
		case float64:
			val = strconv.FormatFloat(t, 'g', 8, 64)
		case int:
			val = strconv.Itoa(t)
		case int64:
			val = strconv.FormatInt(t, 10)
		case string:
			val = t
		default:
			fmt.Printf("%#v %T", v, v)
			panic(v)
		}

		vals.Set(k, val)
	}

	cli, err := client()
	if err != nil {
		panic(err)
	}

	reader := bytes.NewBufferString(vals.Encode())

	r := NewRequest(ioutil.NopCloser(reader))
	r.Params["SCRIPT_FILENAME"] = "/home/tsm/tmp/test.php"
	r.Params["CONTENT_LENGTH"] = strconv.Itoa(reader.Len())
	r.Params["CONTENT_TYPE"] = "application/x-www-form-urlencoded"
	r.Params["REQUEST_METHOD"] = http.MethodPost
	r.KeepConn = true

	res, err := cli.Do(r)
	if err != nil {
		panic(err)
	}

	if err := res.WriteTo(rec, os.Stdout); err != nil {
		panic(err)
	}

	res.Close()

	out, err := httputil.DumpResponse(rec.Result(), true)
	fmt.Println(string(out), err)

	rec.Flush()
}
