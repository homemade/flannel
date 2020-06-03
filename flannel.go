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

type APIClient struct {
	httpClient       *http.Client
	logger           Logger
	debugModeEnabled bool
}

type Logger interface {
	Log(args ...interface{})
}

type FundraiserCoverPhotoReader struct {
	Reader    io.Reader
	MaxSize   int
	bytesRead int
}

var ErrFundraiserCoverPhotoMaxSizeExceeded = errors.New("fundraiser cover photo max size exceeded")

func (r *FundraiserCoverPhotoReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.bytesRead = r.bytesRead + n
	if r.bytesRead > r.MaxSize {
		// if we have exceeded the max size then override the error
		err = ErrFundraiserCoverPhotoMaxSizeExceeded
	}
	return
}

type FacebookError struct {
	Endpoint string

	Status int

	// ErrorMap contains returned Facebook error values.
	// See https://developers.facebook.com/docs/graph-api/using-graph-api/error-handling/
	ErrorMap map[string]interface{}
}

func (e FacebookError) Error() string {
	return fmt.Sprintf("%s %d %v", e.Endpoint, e.Status, e.ErrorMap)
}

func (e FacebookError) ErrorCodes() (code int, subcode int) {
	rawCode := e.ErrorMap["code"]
	if rawCode != nil {
		if f, ok := rawCode.(float64); ok {
			code = int(f)
		}
	}
	rawSubCode := e.ErrorMap["error_subcode"]
	if rawSubCode != nil {
		if f, ok := rawSubCode.(float64); ok {
			subcode = int(f)
		}
	}
	return
}

const CreateFundraiserEndpoint = "https://graph.facebook.com/v2.8/me/fundraisers"

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

const FundraiserCoverPhotoImageMaxSize = (4 * 1024 * 1024) - 1 // fundraiser cover photo images must be less than 4 MB

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

func WithLogger(logger Logger, debug bool) func(*APIClient) error {
	return func(c *APIClient) error {
		c.logger = logger
		c.debugModeEnabled = debug
		return nil
	}
}

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

func WithFundraiserCoverPhotoImage(name string, content io.Reader) func(*multipart.Writer) error {
	return func(w *multipart.Writer) error {
		part, err := w.CreateFormFile("cover_photo", name)
		if err != nil {
			return err
		}
		_, err = io.Copy(part, &FundraiserCoverPhotoReader{Reader: content, MaxSize: FundraiserCoverPhotoImageMaxSize})
		return err
	}
}

func WithFundraiserCoverPhotoURL(name string, content url.URL) func(*multipart.Writer) error {
	return func(w *multipart.Writer) error {
		part, err := w.CreateFormFile("cover_photo", name)
		if err != nil {
			return err
		}
		httpClient := &http.Client{Timeout: time.Second * 20}
		res, err := httpClient.Get(content.String())
		if err != nil {
			return err
		}
		defer res.Body.Close()
		_, err = io.Copy(part, &FundraiserCoverPhotoReader{Reader: res.Body, MaxSize: FundraiserCoverPhotoImageMaxSize})
		return err
	}
}

func WithFundraiserField(name string, value string) func(*multipart.Writer) error {
	return func(w *multipart.Writer) error {
		return w.WriteField(name, value)
	}
}

func IsErrorWithFundraiserCoverPhoto(err error) bool {
	if err == ErrFundraiserCoverPhotoMaxSizeExceeded {
		return true
	}
	if fe, ok := err.(FacebookError); ok {
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
				c.logger.Log(fmt.Sprintf("facebook api %s request to %s returned %d %s\n", req.Method, req.URL.String(), status, string(body)))
			} else {
				c.logger.Log(fmt.Sprintf("facebook api %s request to %s returned %d\n", req.Method, req.URL.String(), status))
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
				err = FacebookError{Endpoint: endpoint, Status: status, ErrorMap: m}
			}
		} else {
			err = fmt.Errorf("invalid response %d", status)
		}
	}
	return
}
