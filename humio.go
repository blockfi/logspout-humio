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
	"log/syslog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
	"unsafe"

	"github.com/gliderlabs/logspout/cfg"
	"github.com/gliderlabs/logspout/router"
)

const (
	// Rfc5424Format is the modern syslog protocol format. https://tools.ietf.org/html/rfc5424
	Rfc5424Format Format = "rfc5424"
	// Rfc3164Format is the legacy BSD syslog protocol format. https://tools.ietf.org/html/rfc3164
	Rfc3164Format Format = "rfc3164"

	defaultFormat     = Rfc5424Format
)

var (
	hostname string
)

// Format represents the RFC spec to use for messages
type Format string


func init() {
	hostname, _ = os.Hostname()
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

func getFormat() (Format, error) {
	switch s := cfg.GetEnvDefault("SYSLOG_FORMAT", string(defaultFormat)); s {
	case string(Rfc5424Format):
		return Rfc5424Format, nil
	case string(Rfc3164Format):
		return Rfc3164Format, nil
	default:
		return defaultFormat, fmt.Errorf("unknown SYSLOG_FORMAT value: %s", s)
	}
}

func getHostname() string {
	content, err := ioutil.ReadFile("/etc/host_hostname")
	if err == nil && len(content) > 0 {
		hostname = strings.TrimRight(string(content), "\r\n")
	} else {
		hostname = cfg.GetEnvDefault("SYSLOG_HOSTNAME", "{{.Container.Config.Hostname}}")
	}
	return hostname
}

func getFieldTemplates(route *router.Route) (*FieldTemplates, error) {
	var err error
	var s string
	var tmpl FieldTemplates

	s = cfg.GetEnvDefault("SYSLOG_PRIORITY", "{{.Priority}}")
	if tmpl.priority, err = template.New("priority").Parse(s); err != nil {
		return nil, err
	}
	debug("setting priority to:", s)

	s = cfg.GetEnvDefault("SYSLOG_TIMESTAMP", "{{.Timestamp}}")
	if tmpl.timestamp, err = template.New("timestamp").Parse(s); err != nil {
		return nil, err
	}
	debug("setting timestamp to:", s)

	s = getHostname()
	if tmpl.hostname, err = template.New("hostname").Parse(s); err != nil {
		return nil, err
	}
	debug("setting hostname to:", s)

	s = cfg.GetEnvDefault("SYSLOG_TAG", "{{.ContainerName}}"+route.Options["append_tag"])
	if tmpl.tag, err = template.New("tag").Parse(s); err != nil {
		return nil, err
	}
	debug("setting tag to:", s)

	s = cfg.GetEnvDefault("SYSLOG_PID", "{{.Container.State.Pid}}")
	if tmpl.pid, err = template.New("pid").Parse(s); err != nil {
		return nil, err
	}
	debug("setting pid to:", s)

	s = cfg.GetEnvDefault("SYSLOG_STRUCTURED_DATA", "")
	if route.Options["structured_data"] != "" {
		s = route.Options["structured_data"]
	}
	if s == "" {
		s = "-"
	} else {
		s = fmt.Sprintf("[%s]", s)
	}
	if tmpl.structuredData, err = template.New("structuredData").Parse(s); err != nil {
		return nil, err
	}
	debug("setting structuredData to:", s)

	s = cfg.GetEnvDefault("SYSLOG_DATA", "{{.Data}}")
	if tmpl.data, err = template.New("data").Parse(s); err != nil {
		return nil, err
	}
	debug("setting data to:", s)

	return &tmpl, nil
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

// FieldTemplates for rendering Syslog messages
type FieldTemplates struct {
	priority       *template.Template
	timestamp      *template.Template
	hostname       *template.Template
	tag            *template.Template
	pid            *template.Template
	structuredData *template.Template
	data           *template.Template
}

// HumioAdapter is an adapter that POSTs logs to an HTTP endpoint
type HumioAdapter struct {
	format     Format
	tmpl       *FieldTemplates
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

	format, err := getFormat()
	if err != nil {
		return nil, err
	}
	debug("setting format to:", format)

	tmpl, err := getFieldTemplates(route)
	if err != nil {
		return nil, err
	}

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
			debug("cannot parse proxy url:", err, proxyUrlString)
			return nil, err
		}
		transport.Proxy = http.ProxyURL(proxyUrl)
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		debug("humio: proxy url:", proxyUrl)
	}

	// Figure out the Humio specifig settings
	humioToken := os.Getenv("HUMIO_INGEST_TOKEN")
	if humioToken == "" {
		return nil, fmt.Errorf("HUMIO_INGEST_TOKEN is required but was not set")
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
		format:     format,
		tmpl:       tmpl,
		route:      route,
		url:        endpointUrl,
		client:     client,
		buffer:     buffer,
		timer:      timer,
		capacity:   capacity,
		timeout:    timeout,
		useGzip:    useGzip,
		crash:      crash,
		humioToken:		humioToken,
		humioIndex:		humioIndex,
		humioSourcetype:	humioSourcetype,
	}, nil
}

func BytesToString(data []byte) string {
	return *(*string)(unsafe.Pointer(&data))
	//	return string(data[:])
}

// Stream implements the router.LogAdapter interface
func (a *HumioAdapter) Stream(logstream chan *router.Message) {
	for {
		select {
		case message := <-logstream:
			if os.Getenv("HUMIO_DISABLE_SYSLOG_FMT") != "" {
				m := &Message{message}
				buf, err := m.Render(a.format, a.tmpl)
				if err != nil {
					log.Println("humio:", err)
					return
				}
				m.Data = BytesToString(buf)
				debug("message: ", m.Data)
			}

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
		if os.Getenv("HUMIO_DOCKER_LABELS") != "" {
			humioMessageEvent.Labels = make(map[string]string)
			for label, value := range m.Container.Config.Labels {
				humioMessageEvent.Labels[strings.Replace(label, ".", "_", -1)] = value
			}
		}
		humioMessage := HumioMessage{
			Time:           m.Time.Format(time.RFC3339Nano),
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
	Time		string `json:"time"`
	Source		string `json:"source"`
	SourceType	string `json:"sourcetype"`
	Hostname	string `json:"host"`
	Event		HumioMessageEvent	`json:"event"`
}


// Message extends router.Message for the syslog standard
type Message struct {
	*router.Message
}

// Render transforms the log message using the Syslog template
func (m *Message) Render(format Format, tmpl *FieldTemplates) ([]byte, error) {
	priority := new(bytes.Buffer)
	if err := tmpl.priority.Execute(priority, m); err != nil {
		return nil, err
	}

	timestamp := new(bytes.Buffer)
	if err := tmpl.timestamp.Execute(timestamp, m); err != nil {
		return nil, err
	}

	hostname := new(bytes.Buffer)
	if err := tmpl.hostname.Execute(hostname, m); err != nil {
		return nil, err
	}

	tag := new(bytes.Buffer)
	if err := tmpl.tag.Execute(tag, m); err != nil {
		return nil, err
	}

	pid := new(bytes.Buffer)
	if err := tmpl.pid.Execute(pid, m); err != nil {
		return nil, err
	}

	structuredData := new(bytes.Buffer)
	if err := tmpl.structuredData.Execute(structuredData, m); err != nil {
		return nil, err
	}

	data := new(bytes.Buffer)
	if err := tmpl.data.Execute(data, m); err != nil {
		return nil, err
	}

	buf := new(bytes.Buffer)
	switch format {
	case Rfc5424Format:
		// notes from RFC:
		// - there is no upper limit for the entire message and depends on the transport in use
		// - the HOSTNAME field must not exceed 255 characters
		// - the TAG field must not exceed 48 characters
		// - the PROCID field must not exceed 128 characters
		fmt.Fprintf(buf, "<%s>1 %s %.255s %.48s %.128s - %s %s\n",
			priority, timestamp, hostname, tag, pid, structuredData, data,
		)
	case Rfc3164Format:
		// notes from RFC:
		// - the entire message must be <= 1024 bytes
		// - the TAG field must not exceed 32 characters
		fmt.Fprintf(buf, "<%s>%s %s %.32s[%s]: %s\n",
			priority, timestamp, hostname, tag, pid, data,
		)
	}

	return buf.Bytes(), nil
}

// Priority returns a syslog.Priority based on the message source
func (m *Message) Priority() syslog.Priority {
	switch m.Message.Source {
	case "stdout":
		return syslog.LOG_USER | syslog.LOG_INFO
	case "stderr":
		return syslog.LOG_USER | syslog.LOG_ERR
	default:
		return syslog.LOG_DAEMON | syslog.LOG_INFO
	}
}

// Hostname returns the os hostname
func (m *Message) Hostname() string {
	return hostname
}

// Timestamp returns the message's syslog formatted timestamp
func (m *Message) Timestamp() string {
	return m.Message.Time.Format(time.RFC3339)
}

// ContainerName returns the message's container name
func (m *Message) ContainerName() string {
	return m.Message.Container.Name[1:]
}

// ContainerNameSplitN returns the message's container name sliced at most "n" times using "sep"
func (m *Message) ContainerNameSplitN(sep string, n int) []string {
	return strings.SplitN(m.ContainerName(), sep, n)
}
