package flannel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"
)

// APIClient represents a HTTP client to the Facebook APIs.
type APIClient struct {
	httpClient       *http.Client
	logger           Logger
	debugModeEnabled bool
}

// Logger is the interface implemented by the APIClient when logging API calls.
type Logger interface {
	Logf(format string, args ...interface{})
}

// The LoggerFunc type is an adapter to allow the use of ordinary functions as Loggers.
// If f is a function with the appropriate signature, LoggerFunc(f) is a Logger that calls f.
type LoggerFunc func(format string, args ...interface{})

// Logf calls f(format, args).
func (f LoggerFunc) Logf(format string, args ...interface{}) {
	f(format, args)
}

type flannelError struct {
	Type int
	Err  error
}

func (e flannelError) Error() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

const (
	errorWithFundraiserCoverPhoto = iota
)

// A RestrictedReader wraps the provided Reader restricting the
// amount of data read to the specified MaxSize of bytes.
// Each call to Read updates BytesRead to reflect the new total.
// If the MaxSize is exceeded an error is returned.
type RestrictedReader struct {
	Reader    io.Reader
	MaxSize   int
	BytesRead int
}

var errMaxSizeExceeded = errors.New("max size exceeded")

func (r *RestrictedReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.BytesRead = r.BytesRead + n
	if r.BytesRead > r.MaxSize {
		// if we have exceeded the max size then override the error
		err = errMaxSizeExceeded
	}
	return
}

// IsMaxSizeExceeded returns true if err is max size exceeded.
func (r *RestrictedReader) IsMaxSizeExceeded(err error) bool {
	return err == errMaxSizeExceeded
}

type facebookError struct {
	Endpoint string

	Status int

	// ErrorMap contains returned Facebook error values.
	// See https://developers.facebook.com/docs/graph-api/using-graph-api/error-handling/
	ErrorMap map[string]interface{}
}

func (e facebookError) Error() string {
	return fmt.Sprintf("%s %d %v", e.Endpoint, e.Status, e.ErrorMap)
}

func (e facebookError) ErrorCodes() (code int, subcode int) {
	f := func(key string) int {
		if v, exists := e.ErrorMap[key]; exists {
			if i, ok := v.(float64); ok {
				return int(i)
			}
		}
		return 0
	}
	return f("code"), f("error_subcode")
}

func (e facebookError) Messages() (message string, errorusertitle string, errorusermsg string) {
	f := func(key string) string {
		if v, exists := e.ErrorMap[key]; exists {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	return f("message"), f("error_user_title"), f("error_user_msg")
}

// Facebook API endpoints.
const (
	CreateFundraiserEndpoint = "https://graph.facebook.com/v2.8/me/fundraisers"
)

// CreateFundraiserParams is the set of parameters required to create a Facebook Fundraiser.
type CreateFundraiserParams struct {

	// AccessToken as provided by Facebook Login for the user creating the fundraiser.
	AccessToken string

	// Charity ID is the Facebook Charity ID
	CharityID string

	// Title of the Facebook Fundraiser, can be up to 70 characters long.
	Title string

	// Description of the Facebook Fundraiser, can be up to 50k characters long.
	Description string

	// Goal in currency’s smallest unit. Fundraisers currently support only whole values, so for currencies with cents like USD,
	// you must round to an integer number and then multiply by 100 to get the value in cents. For zero-decimal currencies like JPY,
	// simply provide the integer amount without multiplying by 100.
	Goal int

	// Currency ISO 4127 code for the goal amount.
	Currency string

	// EndTime is a timestamp of when the fundraiser will stop accepting donations. Must be within 5 years from now.
	EndTime time.Time

	// ExternalID is generated by you to identify the fundraiser in your system.
	ExternalID string
}

// FundraiserCoverPhotoImageMaxSize defines the maximum size for fundraiser cover photo images.
const FundraiserCoverPhotoImageMaxSize = (4 * 1024 * 1024) - 1

// CreateAPIClient creates a new HTTP client to the Facebook APIs with the provided options.
func CreateAPIClient(options ...func(*APIClient) error) (APIClient, error) {
	c := APIClient{
		httpClient: &http.Client{Timeout: time.Second * 20},
	}
	for _, option := range options {
		if err := option(&c); err != nil {
			return c, err
		}
	}
	return c, nil
}

// WithLogger adds logging of API calls on error to the APIClient.
// If the debug flag is set all API calls are logged.
func WithLogger(logger Logger, debug bool) func(*APIClient) error {
	return func(c *APIClient) error {
		c.logger = logger
		c.debugModeEnabled = debug
		return nil
	}
}

// CreateFundraiser creates a new Facebook Fundraiser.
// Required parameters are set with params.
// Optional parameters  are set with options.
func (c APIClient) CreateFundraiser(params CreateFundraiserParams, options ...func(*multipart.Writer) error) (status int, result map[string]interface{}, err error) {

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	// add required fields
	fields := map[string]string{
		"charity_id":      params.CharityID,
		"name":            params.Title,
		"description":     params.Description,
		"goal_amount":     fmt.Sprintf("%d", params.Goal),
		"currency":        params.Currency,
		"end_time":        fmt.Sprintf("%d", params.EndTime.Unix()),
		"external_id":     params.ExternalID,
		"fundraiser_type": "person_for_charity",
	}
	for k, v := range fields {
		err = writer.WriteField(k, v)
		if err != nil {
			break
		}
	}
	if err != nil {
		return 0, nil, err
	}
	// add optional fields
	for _, option := range options {
		if err := option(writer); err != nil {
			return 0, nil, err
		}
	}
	err = writer.Close()
	if err != nil {
		return 0, nil, err
	}
	var req *http.Request
	req, err = http.NewRequest("POST", CreateFundraiserEndpoint, body)
	if err != nil {
		return 0, nil, fmt.Errorf("error preparing request %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+params.AccessToken)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	var res *http.Response
	res, err = c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("error transporting request %v", err)
	}

	return c.readResponse(CreateFundraiserEndpoint, req, res, http.StatusOK)
}

// WithFundraiserCoverPhotoImage adds an optional cover photo image when creating a new Facebook Fundraiser.
func WithFundraiserCoverPhotoImage(name string, content io.Reader) func(*multipart.Writer) error {
	return func(w *multipart.Writer) error {
		part, err := w.CreateFormFile("cover_photo", name)
		if err != nil {
			return flannelError{errorWithFundraiserCoverPhoto, err}
		}
		_, err = io.Copy(part, &RestrictedReader{Reader: content, MaxSize: FundraiserCoverPhotoImageMaxSize})
		if err != nil {
			return flannelError{errorWithFundraiserCoverPhoto, err}
		}
		return nil
	}
}

// WithFundraiserCoverPhotoURL adds an optional cover photo when creating a new Facebook Fundraiser.
func WithFundraiserCoverPhotoURL(name string, content url.URL) func(*multipart.Writer) error {
	return func(w *multipart.Writer) error {
		part, err := w.CreateFormFile("cover_photo", name)
		if err != nil {
			return flannelError{errorWithFundraiserCoverPhoto, err}
		}
		httpClient := &http.Client{Timeout: time.Second * 20}
		res, err := httpClient.Get(content.String())
		if err != nil {
			return flannelError{errorWithFundraiserCoverPhoto, err}
		}
		defer res.Body.Close()
		_, err = io.Copy(part, &RestrictedReader{Reader: res.Body, MaxSize: FundraiserCoverPhotoImageMaxSize})
		if err != nil {
			return flannelError{errorWithFundraiserCoverPhoto, err}
		}
		return nil
	}
}

// WithFundraiserField adds an optional field when creating a new Facebook Fundraiser.
//
// The Facebook Fundraiser API supports the following optional fields:
//
// external_fundraiser_uri - URI of the fundraiser on the external site
//
// external_event_name - Name of the event this fundraiser belongs to
//
// external_event_uri - URI of the event this fundraiser belongs to
//
// external_event_start_time - Unix timestamp of the day when the event takes place
//
func WithFundraiserField(name string, value string) func(*multipart.Writer) error {
	return func(w *multipart.Writer) error {
		return w.WriteField(name, value)
	}
}

// IsErrorWithFundraiserCoverPhoto returns true if err was returned from WithFundraiserCoverPhotoURL option.
func IsErrorWithFundraiserCoverPhoto(err error) bool {
	if e, ok := err.(flannelError); ok {
		return e.Type == errorWithFundraiserCoverPhoto
	}
	if fe, ok := err.(facebookError); ok {
		if fe.Endpoint == CreateFundraiserEndpoint && fe.Status == http.StatusBadRequest {
			code, subCode := fe.ErrorCodes()
			// 100 1366046 Your photos couldn't be uploaded. Photos should be smaller than 4 MB and saved as JPG, PNG, GIF, TIFF, HEIF or WebP files.
			// 100 1366055 Your photo couldn't be uploaded due to restrictions on image dimensions. Photos should be less than 30,000 pixels in any dimension, and less than 80,000,000 pixels in total size.
			if code == 100 && (subCode == 1366046 || subCode == 1366055) {
				return true
			}
		}
	}
	return false
}

// ErrorMessages extracts any Facebook error messages from err.
// See https://developers.facebook.com/docs/graph-api/using-graph-api/error-handling/
func ErrorMessages(err error) (message string, errorusertitle string, errorusermsg string) {
	if fe, ok := err.(facebookError); ok {
		return fe.Messages()
	}
	return err.Error(), "", ""
}

// ErrorMessages extracts any Facebook error codes from err.
// See https://developers.facebook.com/docs/graph-api/using-graph-api/error-handling/
func ErrorCodes(err error) (code int, subcode int) {
	if fe, ok := err.(facebookError); ok {
		return fe.ErrorCodes()
	}
	return 0, 0
}

func (c APIClient) readResponse(endpoint string, req *http.Request, res *http.Response, expectedstatus int) (status int, result map[string]interface{}, err error) {
	var body []byte
	if res != nil {
		status = res.StatusCode
		if res.ContentLength > 0 || res.ContentLength == -1 { // -1 represents unknown content length
			body, err = ioutil.ReadAll(res.Body)
			if err == nil {
				// Defer closing of underlying connection so it can be re-used...
				defer res.Body.Close()
			}
		}
	}
	defer func() {
		if c.logger != nil && (c.debugModeEnabled || err != nil) {
			if len(body) > 0 {
				c.logger.Logf("facebook api %s request to %s returned %d %s\n", req.Method, req.URL.String(), status, string(body))
			} else {
				c.logger.Logf("facebook api %s request to %s returned %d\n", req.Method, req.URL.String(), status)
			}
		}
	}()
	if err != nil {
		err = fmt.Errorf("error reading response %v", err)
	}
	result = make(map[string]interface{})
	err = json.Unmarshal(body, &result)
	if err != nil {
		err = fmt.Errorf("error parsing response %v", err)
	}
	if status != expectedstatus {
		if e, exists := result["error"]; exists {
			if m, ok := e.(map[string]interface{}); ok {
				err = facebookError{Endpoint: endpoint, Status: status, ErrorMap: m}
			}
		} else {
			err = fmt.Errorf("invalid response %d", status)
		}
	}
	return
}
