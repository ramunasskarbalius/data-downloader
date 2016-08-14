package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gosuri/uilive"

	"math/rand" // for debug purposes
)

var debugging = false // if true, debug messages will be shown

var (
	res          Resumer
	outputWriter *bufio.Writer

	resumerSuffix string = ".audisto_"
)

// flags
var (
	username string
	password string
	crawl    uint64

	noDetails bool
	output    string
	noResume  bool
)

// progress bar elements
var (
	progressIndicator *uilive.Writer
	progressStatus    string

	timeoutCount int
	errorCount   int

	averageTimePer1000 float64 = 5
)

type Resumer struct {
	OutputFilename string `json:"outputFilename"`

	chunkSize     int64
	DoneElements  int64 `json:"doneElements"`
	TotalElements int64 `json:"totalElements"`
	NoDetails     bool  `json:"noDetails"`

	httpClient http.Client
}

// chunk is used to get unmarshal the json containing the total number of chunks
type chunk struct {
	Chunk struct {
		Total int64 `json:"total"`
		Page  int   `json:"page"`
		Size  int   `json:"size"`
	} `json:"chunk"`
}

// init parses flags and sets everything up
func init() {

	flag.StringVar(&username, "username", "", "API Username (required)")
	flag.StringVar(&password, "password", "", "API Password (required)")
	flag.Uint64Var(&crawl, "crawl", 0, "ID of the crawl to download (required)")

	flag.BoolVar(&noDetails, "no-details", false, "If passed, details in API request is set to 0 else")
	flag.StringVar(&output, "output", "", "Path for the output file")
	flag.BoolVar(&noResume, "no-resume", false, "If passed, download starts again, else the download is resumed")

	flag.Usage = usage
	flag.Parse()

	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	output = strings.TrimSpace(output)

	// Check for non-valid flags
	usernameIsNull := username == ""
	passwordIsNull := password == ""
	crawlIsNull := crawl == 0

	if usernameIsNull || passwordIsNull || crawlIsNull {
		usage()
		os.Exit(0)
	}

	// stdout or output file ?
	if output == "" {
		outputWriter = bufio.NewWriter(os.Stdout)

		var err error

		res = Resumer{}

		res.TotalElements, err = totalElements()
		if err != nil {
			fmt.Println(err)
			return
		}
		res.OutputFilename = output
		res.NoDetails = noDetails
	} else {

		errOutput, errResumer := fExists(output), fExists(output+resumerSuffix)
		startAnew := errOutput != nil && errResumer != nil

		// if don't resume, create new set
		if noResume || startAnew {

			if startAnew && !noResume {
				fmt.Println("No download to resume; stating new.")
			}

			// if resume, check if output file exists
			if errOutput == nil {
				fmt.Println("File already exists; please resume removing --no-resume, delete or specify another output filename.")
				return
			}

			var err error

			res = Resumer{}

			res.TotalElements, err = totalElements()
			if err != nil {
				fmt.Println(err)
				return
			}
			res.OutputFilename = output
			res.NoDetails = noDetails

			err = res.PersistConfig()
			if err != nil {
				panic(err)
			}

			// create new outputFile
			newFile, err := os.Create(output)
			if err != nil {
				panic(err)
			}
			outputWriter = bufio.NewWriter(newFile)
		} else {
			// if resume, check if output file exists
			if errOutput != nil {
				fmt.Println(fmt.Sprintf("Cannot resume; %q file does not exist: use --no-resume to create new.", output))
				return
			}
			// if resume, check if resume file exists
			if errResumer != nil {
				fmt.Println(fmt.Sprintf("Cannot resume; resumer file %v does not exist: ", output+resumerSuffix))
				return
			}

			resumerFile, err := ioutil.ReadFile(output + resumerSuffix)
			if err != nil {
				panic(fmt.Sprintf("Resumer file error: %v\n", err))
			}
			err = json.Unmarshal(resumerFile, &res)
			if err != nil {
				panic(fmt.Sprintf("Resumer file error: %v\n", err))
			}

			// open outputFile
			existingFile, err := os.OpenFile(output, os.O_WRONLY|os.O_APPEND, 0777)
			if err != nil {
				panic(err)
			}
			outputWriter = bufio.NewWriter(existingFile)

			// read and validate resumer file
			// read and validate output file
			// check last id of the last write batch

			if res.NoDetails != noDetails {
				fmt.Println(fmt.Sprintf("Warning! This file was begun with --no-details=%v; continuing with --no-details=%v will break the file.", res.NoDetails, noDetails))
				return
			}

		}

	}

	// set chunkSize to 10000
	res.chunkSize = 10000
}

func main() {

	if output != "" {
		progressIndicator = uilive.New()
		progressIndicator.Start()
		go progressLoop()
	}

	debug(username, password, crawl)

	debugf("%#v\n", res)
MainLoop:
	for {
		var startTime time.Time = time.Now()
		var processedLines int64 = 0

		//res.chunkSize = int64(random(1000, 10000)) // debug; random chunk size

		progressPerc := res.progress()
		updateStatus(fmt.Sprintf("%.1f%% of %v pages", progressPerc, res.TotalElements))
		debugf("Progress: %.1f %%", progressPerc)
		if res.DoneElements == res.TotalElements {
			updateStatus("@@@ COMPLETED 100% @@@")

			debug("@@@ COMPLETED 100% @@@")
			debugf("removing %v", output+resumerSuffix)
			if output != "" {
				os.Remove(output + resumerSuffix)

				progressIndicator.Stop()
			}
			return
		}

		remainingElements := res.TotalElements - res.DoneElements
		if remainingElements < res.chunkSize {
			res.chunkSize = remainingElements
		}

		debugf("Calling next chunk")
		var chunk []byte
		var statusCode int
		var skip int64
		err := retry(5, 10, func() error {
			var err error
			chunk, statusCode, skip, err = res.nextChunk()
			return err
		})

		if err != nil {
			debugf("Too many failures while calling next chunk; %v\n", err)
			fmt.Println("Network error; please check your connection to the internet and resume download.")
			return
		}
		debugf("Next chunk obtained")
		debugf("statusCode: %v", statusCode)

		// check status code

		if statusCode != 200 {
			errorCount += 1
		}

		switch {
		case statusCode == 429:
			{
				time.Sleep(time.Second * 30)
				continue MainLoop
			}
		case statusCode >= 400 && statusCode < 500:
			{
				switch statusCode {
				case 403:
					{
						fmt.Println("Access denied. Wrong credentials?")
						return
					}
				case 404:
					{
						fmt.Println("Not found. Correct crawl ID?")
						return
					}
				default:
					{
						fmt.Printf("\nUnknown error occured (code %v).\n", statusCode)
						return
					}
				}
			}
		case statusCode == 504:
			{
				timeoutCount += 1
				if timeoutCount >= 3 {
					if (res.chunkSize - 1000) > 0 {
						res.chunkSize = res.chunkSize - 1000
						timeoutCount = 0
					}
				}
				time.Sleep(time.Second * 30)
				continue MainLoop
			}
		case statusCode >= 500 && statusCode < 600:
			{
				time.Sleep(time.Second * 30)
				continue MainLoop
			}
		}

		scanner := bufio.NewScanner(bytes.NewReader(chunk))
		debugf("chunk bytes len: %v", len(chunk))

		// is DoneElements == 0, don't skip first line
		if res.DoneElements == 0 {
			scanner.Scan()
			outputWriter.Write(append(scanner.Bytes(), []byte("\n")...))
		}

		// skip lines
		for i := int64(0); i < skip; i++ {
			scanner.Scan()
			debugf("skipping this row: \n%s ", scanner.Text())
		}

		for scanner.Scan() {
			outputWriter.Write(append(scanner.Bytes(), []byte("\n")...))
			res.DoneElements += 1
			processedLines += 1
		}

		outputWriter.Flush()
		debugf("res.DoneElements = %v", res.DoneElements)
		res.PersistConfig()

		itTook := time.Since(startTime)
		temp := big.NewFloat(0).Quo(big.NewFloat(itTook.Seconds()), big.NewFloat(0).Quo(big.NewFloat(0).SetInt(big.NewInt(processedLines)), big.NewFloat(1000)))
		lastSpeed, _ := temp.Float64()
		SMOOTHING_FACTOR := 0.005
		averageSpeed := big.NewFloat(0).Add(big.NewFloat(0).Mul(big.NewFloat(SMOOTHING_FACTOR), big.NewFloat(lastSpeed)), big.NewFloat(0).Mul(big.NewFloat(0).Sub(big.NewFloat(0).SetInt(big.NewInt(1)), big.NewFloat(SMOOTHING_FACTOR)), big.NewFloat(averageTimePer1000)))
		averageTimePer1000, _ = averageSpeed.Float64()

		if err := scanner.Err(); err != nil {
			errorCount += 1
			fmt.Println("error wrile scanning chunk: ", err)
			return
		}

	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: audistoDownloader [flags]
Flags:
  username    API Username (required)
  password    API Password (required)
  crawl       ID of the crawl to download (required)
  no-details  If passed, details in API request is set to 0 else
  output      Path for the output file
  no-resume   If passed, download starts again, else the download is resumed
`)
}

func debugf(format string, a ...interface{}) (n int, err error) {
	if debugging {
		return fmt.Printf("\n"+format+"\n", a...)
	}
	return 0, nil
}
func debug(a ...interface{}) (n int, err error) {
	if debugging {
		return fmt.Println(a...)
	}
	return 0, nil
}

// fExists returns nil if path is an existing file/folder
func fExists(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return err
	}
	return nil
}

func random(min, max int) int {
	rand.Seed(time.Now().Unix())
	return rand.Intn(max-min) + min
}

// chs() outputs a string made of c repeated n times
func chs(n int, c string) string {
	var s string
	for i := 0; i < n; i++ {
		s = s + c
	}
	return s
}

func retry(attempts int, sleep int, callback func() error) (err error) {
	for i := 0; ; i++ {
		err = callback()
		if err == nil {
			return nil
		}

		if i >= (attempts - 1) {
			break
		}

		errorCount += 1
		time.Sleep(time.Duration(sleep) * time.Second)
		debugf("Something failed, retrying;")
	}
	return fmt.Errorf("Abandoned after %d attempts, last error: %s", attempts, err)
}

func (r *Resumer) nextChunkNumber() (nextChunkNumber, skipNRows int64) {

	if r.DoneElements == 0 {
		nextChunkNumber = 0
		skipNRows = 0
		return
	}

	skipNRows = r.DoneElements % r.chunkSize
	nextChunkNumberFloat, _ := math.Modf(float64(r.DoneElements) / float64(r.chunkSize))

	nextChunkNumber = int64(nextChunkNumberFloat)
	return
}

func (r *Resumer) nextChunk() ([]byte, int, int64, error) {

	nextChunkNumber, skipNRows := r.nextChunkNumber()

	if r.DoneElements > 0 {
		skipNRows += 1
	}

	path := fmt.Sprintf("/2.0/crawls/%v/pages", crawl)
	method := "GET"

	headers := http.Header{}
	bodyParameters := url.Values{}

	queryParameters := url.Values{}
	if noDetails {
		queryParameters.Add("deep", "0")
	} else {
		queryParameters.Add("deep", "1")
	}
	queryParameters.Add("chunk", strconv.FormatInt(nextChunkNumber, 10))
	queryParameters.Add("chunk_size", strconv.FormatInt(r.chunkSize, 10))
	queryParameters.Add("output", "tsv")

	body, statusCode, err := r.fetchRawChunk(path, method, headers, queryParameters, bodyParameters)
	if err != nil {
		return []byte(""), 0, 0, err
	}

	return body, statusCode, skipNRows, nil
}

func (r *Resumer) fetchRawChunk(path string, method string, headers http.Header, queryParameters url.Values, bodyParameters url.Values) ([]byte, int, error) {

	domain := fmt.Sprintf("https://%s:%s@api.audisto.com", username, password)
	requestURL, err := url.Parse(domain)
	if err != nil {
		return []byte(""), 0, err
	}
	requestURL.Path = path
	requestURL.RawQuery = queryParameters.Encode()

	if method != "GET" && method != "POST" && method != "PATCH" && method != "DELETE" {
		return []byte(""), 0, fmt.Errorf("Method not supported: %v", method)
	}

	debugf("request url: %s", requestURL.String())
	request, err := http.NewRequest(method, requestURL.String(), bytes.NewBufferString(bodyParameters.Encode()))
	if err != nil {
		return []byte(""), 0, fmt.Errorf("Failed to get the URL %s: %s", requestURL, err)
	}
	request.Header = headers
	request.Header.Add("Content-Length", strconv.Itoa(len(bodyParameters.Encode())))

	request.Header.Add("Connection", "Keep-Alive")
	request.Header.Add("Accept-Encoding", "gzip, deflate")
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	response, err := r.httpClient.Do(request)
	if err != nil {
		return []byte(""), 0, fmt.Errorf("Failed to get the URL %s: %s", requestURL, err)
	}

	defer response.Body.Close()

	var responseReader io.ReadCloser
	switch response.Header.Get("Content-Encoding") {
	case "gzip":
		decompressedBodyReader, err := gzip.NewReader(response.Body)
		if err != nil {
			return []byte(""), response.StatusCode, err
		}
		responseReader = decompressedBodyReader
		defer responseReader.Close()
	default:
		responseReader = response.Body
	}

	responseBody, err := ioutil.ReadAll(responseReader)
	if err != nil {
		return []byte(""), response.StatusCode, err
	}

	return responseBody, response.StatusCode, nil
}

func totalElements() (int64, error) {
	var body []byte
	var statusCode int
	err := retry(5, 1, func() error {
		var err error
		body, statusCode, err = res.fetchTotalElements()
		if err != nil {
			return err
		}
		if statusCode != 200 {
			err = fmt.Errorf("error while trying to get total number of elements; statusCode %v", statusCode)
		}
		return err
	})
	if err != nil {
		return 0, err
	}

	var firstChunk chunk
	err = json.Unmarshal(body, &firstChunk)
	if err != nil {
		return 0, err
	}

	return firstChunk.Chunk.Total, nil
}

func (r *Resumer) fetchTotalElements() ([]byte, int, error) {

	path := fmt.Sprintf("/2.0/crawls/%v/pages", crawl)
	method := "GET"

	headers := http.Header{}
	bodyParameters := url.Values{}

	queryParameters := url.Values{}
	queryParameters.Add("deep", "0")
	queryParameters.Add("chunk", "0")
	queryParameters.Add("chunk_size", "1")
	queryParameters.Add("output", "json")

	body, statusCode, err := r.fetchRawChunk(path, method, headers, queryParameters, bodyParameters)
	if err != nil {
		return []byte(""), 0, err
	}

	return body, statusCode, nil
}

// PersistConfig saves the accounts to file
func (r *Resumer) PersistConfig() error {
	// save config to file only if not printing to stdout
	if output == "" {
		return nil
	}

	config, err := json.MarshalIndent(r, "", "	")
	if err != nil {
		return err
	}

	// create {{output}}.audisto_ file (keeps track of progress etc.)
	err = ioutil.WriteFile(output+resumerSuffix, config, 0644)
	if err != nil {
		return err
	}
	return nil
}

// progress outputs the progress percentage
func (r *Resumer) progress() *big.Float {
	var progressPerc *big.Float = big.NewFloat(0.0)
	if res.TotalElements > 0 && res.DoneElements > 0 {
		progressPerc = big.NewFloat(0).Quo(big.NewFloat(100), big.NewFloat(0).Quo(big.NewFloat(0).SetInt64(res.TotalElements), big.NewFloat(0).SetInt64(res.DoneElements)))
	}
	return progressPerc
}

// updateStatus sets the first part of the progress bar message
func updateStatus(s string) {
	// TODO: add a mutex?
	progressStatus = s
}

// progress animation
func progressLoop() {
	var n int = 0
	var max int = 10
	for {

		ETAuint64, _ := big.NewFloat(0).Quo(big.NewFloat(0).Quo(big.NewFloat(0).Sub(big.NewFloat(0).SetInt64(res.TotalElements), big.NewFloat(0).SetInt64(res.DoneElements)), big.NewFloat(1000)), big.NewFloat(averageTimePer1000)).Uint64()
		ETAtime := time.Duration(ETAuint64) * time.Millisecond * 110
		ETAstring := ETAtime.String()

		progressMessage := progressStatus + chs(n, ".") + chs(max-n, "*")
		progressMessage = progressMessage + fmt.Sprintf(" | ETA %v |", ETAstring)
		progressMessage = progressMessage + fmt.Sprintf(" Chunk size %v |", res.chunkSize)
		progressMessage = progressMessage + fmt.Sprintf(" %v timeouts |", timeoutCount)
		progressMessage = progressMessage + fmt.Sprintf(" %v errors |", errorCount)

		fmt.Fprintln(progressIndicator, progressMessage)
		time.Sleep(time.Millisecond * 500)

		n += 1
		if n >= max {
			n = 0
		}
	}
}
