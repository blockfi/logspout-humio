package humio

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gliderlabs/logspout/router"
)

func init() {
	router.AdapterFactories.Register(NewHumioAdapter, "humio")
}

func debug(v ...interface{}) {
	if os.Getenv("DEBUG") != "" {
		log.Println(v...)
	}
}

func die(v ...interface{}) {
	panic(fmt.Sprintln(v...))
}

func getStringParameter(
	options map[string]string, parameterName string, dfault string) string {

	if value, ok := options[parameterName]; ok {
		return value
	} else {
		return dfault
	}
}

func getIntParameter(
	options map[string]string, parameterName string, dfault int) int {

	if value, ok := options[parameterName]; ok {
		valueInt, err := strconv.Atoi(value)
		if err != nil {
			debug("humio: invalid value for parameter:", parameterName, value)
			return dfault
		} else {
			return valueInt
		}
	} else {
		return dfault
	}
}

func getDurationParameter(
	options map[string]string, parameterName string,
	dfault time.Duration) time.Duration {

	if value, ok := options[parameterName]; ok {
		valueDuration, err := time.ParseDuration(value)
		if err != nil {
			debug("humio: invalid value for parameter:", parameterName, value)
			return dfault
		} else {
			return valueDuration
		}
	} else {
		return dfault
	}
}

func dial(netw, addr string) (net.Conn, error) {
	dial, err := net.Dial(netw, addr)
	if err != nil {
		debug("humio: new dial", dial, err, netw, addr)
	} else {
		debug("humio: new dial", dial, netw, addr)
	}
	return dial, err
}

// HumioAdapter is an adapter that POSTs logs to an HTTP endpoint
type HumioAdapter struct {
	route             *router.Route
	url               string
	client            *http.Client
	buffer            []*router.Message
	timer             *time.Timer
	capacity          int
	timeout           time.Duration
	totalMessageCount int
	bufferMutex       sync.Mutex
	useGzip           bool
	crash		  bool
	humioToken	  string
	humioIndex	  string
	humioSourcetype   string
}

// NewHumioAdapter creates an HumioAdapter
func NewHumioAdapter(route *router.Route) (router.LogAdapter, error) {

	// Figure out the URI and create the HTTP client
	path := "/api/v1/ingest/hec"
	if os.Getenv("HUMIO_HEC_PATH") != "" {
		path = os.Getenv("HUMIO_HEC_PATH")
	}

	endpointUrl := fmt.Sprintf("https://%s%s", route.Address, path)
	debug("humio: url:", endpointUrl)
	transport := &http.Transport{}
	transport.Dial = dial

	// Figure out if we need a proxy
	defaultProxyUrl := ""
	proxyUrlString := getStringParameter(route.Options, "http.proxy", defaultProxyUrl)
	if proxyUrlString != "" {
		proxyUrl, err := url.Parse(proxyUrlString)
		if err != nil {
			die("", "humio: cannot parse proxy url:", err, proxyUrlString)
		}
		transport.Proxy = http.ProxyURL(proxyUrl)
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		debug("humio: proxy url:", proxyUrl)
	}

	// Figure out the Humio specifig settings
	humioToken := os.Getenv("HUMIO_INGEST_TOKEN")
	if humioToken == "" {
		die("", "humio: HUMIO_INGEST_TOKEN is required but was not set")
	}

	if os.Getenv("HUMIO_TLS") == "false" {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	humioIndex := "main"
        if os.Getenv("HUMIO_INDEX") != "" {
		humioIndex = os.Getenv("HUMIO_INDEX")
	}

        humioSourcetype := "docker"
        if os.Getenv("HUMIO_SOURCETYPE") != "" {
                humioSourcetype = os.Getenv("HUMIO_SOURCETYPE")
        }

	// Create the client
	client := &http.Client{Transport: transport}

	// Determine the buffer capacity
	defaultCapacity := 100
	capacity := getIntParameter(route.Options, "http.buffer.capacity", defaultCapacity)
	if capacity < 1 || capacity > 10000 {
		debug("humio: non-sensical value for parameter: http.buffer.capacity",
			capacity, "using default:", defaultCapacity)
		capacity = defaultCapacity
	}
	buffer := make([]*router.Message, 0, capacity)

	// Determine the buffer timeout
	defaultTimeout, _ := time.ParseDuration("1000ms")
	timeout := getDurationParameter(route.Options, "http.buffer.timeout", defaultTimeout)
	timeoutSeconds := timeout.Seconds()
	if timeoutSeconds < .1 || timeoutSeconds > 600 {
		debug("humio: non-sensical value for parameter: http.buffer.timeout",
			timeout, "using default:", defaultTimeout)
		timeout = defaultTimeout
	}
	timer := time.NewTimer(timeout)

	// Figure out whether we should use GZIP compression
	useGzip := true
	useGZipString := getStringParameter(route.Options, "http.gzip", "false")
	if useGZipString == "true" {
		useGzip = true
		debug("humio: gzip compression enabled")
	}

	// Should we crash on an error or keep going?
	var crash = false
	if os.Getenv("DEBUG") != "" { crash = true } else { crash = false }
	crashString := getStringParameter(route.Options, "http.crash", "true")
	if crashString == "false" {
		crash = false
		debug("humio: don't crash, keep going")
	}

	// Make the HTTP adapter
	return &HumioAdapter{
		route:    route,
		url:      endpointUrl,
		client:   client,
		buffer:   buffer,
		timer:    timer,
		capacity: capacity,
		timeout:  timeout,
		useGzip:  useGzip,
		crash:    crash,
		humioToken:		humioToken,
		humioIndex:		humioIndex,
		humioSourcetype:	humioSourcetype,
	}, nil
}

// Stream implements the router.LogAdapter interface
func (a *HumioAdapter) Stream(logstream chan *router.Message) {
	for {
		select {
		case message := <-logstream:

			// Append the message to the buffer
			a.bufferMutex.Lock()
			a.buffer = append(a.buffer, message)
			a.bufferMutex.Unlock()

			// Flush if the buffer is at capacity
			if len(a.buffer) >= cap(a.buffer) {
				a.flushHttp("full")
			}
		case <-a.timer.C:

			// Timeout, flush
			a.flushHttp("timeout")
		}
	}
}

// Flushes the accumulated messages in the buffer
func (a *HumioAdapter) flushHttp(reason string) {

	// Stop the timer and drain any possible remaining events
	a.timer.Stop()
	select {
	case <-a.timer.C:
	default:
	}

	// Reset the timer when we are done
	defer a.timer.Reset(a.timeout)

	// Return immediately if the buffer is empty
	if len(a.buffer) < 1 {
		return
	}

	// Capture the buffer and make a new one
	a.bufferMutex.Lock()
	buffer := a.buffer
	a.buffer = make([]*router.Message, 0, a.capacity)
	a.bufferMutex.Unlock()

	// Create JSON representation of all messages
	messages := make([]string, 0, len(buffer))
	for i := range buffer {
		m := buffer[i]
		humioMessageEvent := HumioMessageEvent{Message: m.Data}
		if os.Getenv("HUMIO_DOCKER_LABELS") != "" && len(m.Container.Config.Labels) > 0 {
			humioMessageEvent.Labels = make(map[string]string)
			for label, value := range m.Container.Config.Labels {
				humioMessageEvent.Labels[strings.Replace(label, ".", "_", -1)] = value
			}
		}
		humioMessage := HumioMessage{
			Time:           m.Time.Unix(), //TODO: m.Time.Format(time.RFC3339Nano), https://github.com/rickalm/logspout-gelf/blob/master/gelf.go#L54
			Hostname:	m.Container.Config.Hostname,
			Source:		m.Source,
			SourceType:	a.humioSourcetype,
			Event:		humioMessageEvent,
		}
		message, err := json.Marshal(humioMessage)
		if err != nil {
			debug("flushHttp - Error encoding JSON: ", err)
			continue
		}
		messages = append(messages, string(message))
	}

	// Glue all the JSON representations together into one payload to send
	payload := strings.Join(messages, "\n")

	go func() {

		// Create the request and send it on its way
		request := createRequest(a.url, a.useGzip, a.humioToken, payload)
		start := time.Now()
		response, err := a.client.Do(request)
		if err != nil {
			debug("humio - error on client.Do:", err, a.url)
			// TODO now what?
			if a.crash {
				die("humio - error on client.Do:", err, a.url)
			} else {
				debug("humio: error on client.Do:", err)
			}
		} else if response.StatusCode != 200 {
			debug("humio: response not 200 but", response.StatusCode)
			// TODO now what?
			if a.crash {
				die("humio: response not 200 but", response.StatusCode)
			}
		}

		// Make sure the entire response body is read so the HTTP
		// connection can be reused
		if response != nil {
			io.Copy(ioutil.Discard, response.Body)
			response.Body.Close()
		}

		// Bookkeeping, logging
		timeAll := time.Since(start)
		a.totalMessageCount += len(messages)
		debug("humio: flushed:", reason, "messages:", len(messages),
			"in:", timeAll, "total:", a.totalMessageCount)
	}()
}

// Create the request based on whether GZIP compression is to be used
func createRequest(url string, useGzip bool, humioToken string, payload string) *http.Request {
	var request *http.Request
	if useGzip {
		gzipBuffer := new(bytes.Buffer)
		gzipWriter := gzip.NewWriter(gzipBuffer)
		_, err := gzipWriter.Write([]byte(payload))
		if err != nil {
			die("humio: unable to write to GZIP writer:", err)
		}
		err = gzipWriter.Close()
		if err != nil {
			die("humio: unable to close GZIP writer:", err)
		}
		request, err = http.NewRequest("POST", url, gzipBuffer)
		if err != nil {
			die("", "humio: error on http.NewRequest:", err, url)
		}
		request.Header.Set("Content-Encoding", "gzip")
	} else {
		var err error
		request, err = http.NewRequest("POST", url, strings.NewReader(payload))
		if err != nil {
			die("", "humio: error on http.NewRequest:", err, url)
		}
	}

	if (humioToken != "") {
		request.Header.Set("Authorization", "Bearer " + humioToken)
        }

	request.Header.Set("Content-Type", "text/plain; charset=utf-8")

	return request
}

type HumioMessageEvent struct {
        Message         string			`json:"message"`
	Labels		map[string]string	`json:"labels"`
}

// HumioMessage is a simple JSON representation of the log message.
type HumioMessage struct {
	Time		int64 `json:"time"`
	Source		string `json:"source"`
	SourceType	string `json:"sourcetype"`
	Hostname	string `json:"host"`
	Event		HumioMessageEvent	`json:"event"`
}
