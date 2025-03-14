// (C) 2014 Cybozu.  All rights reserved.
// Use of this source code is governed by a BSD-style license
// that can be found in the LICENSE file.

package kintone

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	NAME            = "go-kintone"
	VERSION         = "0.3.0"
	DEFAULT_TIMEOUT = time.Second * 600 // Default value for App.Timeout
)

// Library internal errors.
var (
	ErrTimeout         = errors.New("Timeout")
	ErrInvalidResponse = errors.New("Invalid Response")
	ErrTooMany         = errors.New("Too many records")
)

// Server-side errors.
type AppError struct {
	HttpStatus     string `json:"-"`       // e.g. "404 NotFound"
	HttpStatusCode int    `json:"-"`       // e.g. 404
	Message        string `json:"message"` // Human readable message.
	Id             string `json:"id"`      // A unique error ID.
	Code           string `json:"code"`    // For machines.
	Errors         string `json:"errors"`  // Error Description.
}

type AppFormFields struct {
	Properties interface{} `json:"properties"`
}

func (e *AppError) Error() string {
	if len(e.Message) == 0 {
		return "HTTP error: " + e.HttpStatus
	}
	return fmt.Sprintf("AppError: %d [%s] %s (%s) %s",
		e.HttpStatusCode, e.Code, e.Message, e.Id, e.Errors)
}

type UpdateKeyField interface {
	JSONValue() interface{}
}

type UpdateKey struct {
	FieldCode string
	Field     UpdateKeyField
}

func (f UpdateKey) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"field": f.FieldCode,
		"value": f.Field.JSONValue(),
	})
}

// App provides kintone application API client.
//
// You need to provide Domain, User, Password, and AppId.
// You can also use an API token instead of user/password.
// If you require specialized client settings, for instance
// need to specify a client certificate, proxy settings, or
// are using Google AppEngine, you can build an *http.Client
// instance and supply it to Client.
//
// ex: Google AppEngine
//
//	import (
//		"appengine"
//		"appengine/urlfetch"
//		"github.com/kintone-labs/go-kintone"
//		"net/http"
//	)
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//		c := appengine.NewContext(r)
//		app := &kintone.App{Client: urlfetch.Client(c)}
//		...
//	}
//
// ex: proxy
//
//	import (
//		"net/http"
//		"net/url"
//		"github.com/kintone-labs/go-kintone"
//	)
//
//	func main() {
//		proxyURL, _ := url.Parse("https://proxy.example.com")
//		transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
//		client := &http.Client{Transport: transport}
//		app := &kintone.App{Client: client}
//		...
//	}
//
// Errors returned by the methods of App may be one of *AppError,
// ErrTimeout, ErrInvalidResponse, or ErrTooMany.
type App struct {
	Domain            string        // domain name.  ex: "sample.cybozu.com", "sample.kintone.com", "sample.cybozu.cn"
	User              string        // User account for API.
	Password          string        // User password for API.
	AppId             uint64        // application ID.
	Client            *http.Client  // Specialized client.
	Timeout           time.Duration // Timeout for API responses.
	ApiToken          string        // API token.
	GuestSpaceId      uint64        // guest space ID.
	token             string        // auth token.
	basicAuth         bool          // true to use Basic Authentication.
	basicAuthUser     string        // User name for Basic Authentication.
	basicAuthPassword string        // Password for Basic Authentication.
	extUserAgent      string        // User-agent request header string
}

// SetBasicAuth enables use of HTTP basic authentication for access
// to kintone.
func (app *App) SetBasicAuth(user, password string) {
	app.basicAuth = true
	app.basicAuthUser = user
	app.basicAuthPassword = password
}

// HasBasicAuth indicate authentication is basic or not
func (app *App) HasBasicAuth() bool {
	return app.basicAuth
}

// GetBasicAuthUser return username string for basic authentication
func (app *App) GetBasicAuthUser() string {
	return app.basicAuthUser
}

// GetBasicAuthPassword return password string for basic authentication
func (app *App) GetBasicAuthPassword() string {
	return app.basicAuthPassword
}

// SetUserAgentHeader set custom user-agent header for http request
func (app *App) SetUserAgentHeader(userAgentHeader string) {
	app.extUserAgent = userAgentHeader
}

// GetUserAgentHeader get user-agent header string
func (app *App) GetUserAgentHeader() string {
	userAgent := NAME + "/" + VERSION
	if len(app.extUserAgent) > 0 {
		return userAgent + " " + app.extUserAgent
	}
	return userAgent
}

func (app *App) createUrl(api string, query string) url.URL {
	path := fmt.Sprintf("/k/v1/%s.json", api)
	if app.GuestSpaceId > 0 {
		path = fmt.Sprintf("/k/guest/%d/v1/%s.json", app.GuestSpaceId, api)
	}

	resultUrl := url.URL{
		Scheme: "https",
		Host:   app.Domain,
		Path:   path,
	}

	if len(query) > 0 {
		resultUrl.RawQuery = query
	}
	return resultUrl
}

func (app *App) setAuth(request *http.Request) {
	if app.basicAuth {
		request.SetBasicAuth(app.basicAuthUser, app.basicAuthPassword)
	}

	if len(app.ApiToken) > 0 {
		request.Header.Set("X-Cybozu-API-Token", app.ApiToken)
	}

	if len(app.User) > 0 && len(app.Password) > 0 {
		request.Header.Set("X-Cybozu-Authorization", base64.StdEncoding.EncodeToString(
			[]byte(app.User+":"+app.Password)))
	}
}

// NewRequest create a request connect to kintone api.
func (app *App) NewRequest(method, url string, body io.Reader) (*http.Request, error) {
	bodyData := io.Reader(nil)
	if body != nil {
		bodyData = body
	}

	request, err := http.NewRequest(method, url, bodyData)
	if err != nil {
		return nil, err
	}

	request.Header.Set("User-Agent", app.GetUserAgentHeader())

	if method != "GET" {
		request.Header.Set("Content-Type", "application/json")
	}

	app.setAuth(request)

	return request, nil
}

func (app *App) newRequest(method, api string, body io.Reader) (*http.Request, error) {
	if len(app.token) == 0 {
		app.token = base64.StdEncoding.EncodeToString(
			[]byte(app.User + ":" + app.Password))
	}

	var path string
	if app.GuestSpaceId == 0 {
		path = fmt.Sprintf("/k/v1/%s.json", api)
	} else {
		path = fmt.Sprintf("/k/guest/%d/v1/%s.json", app.GuestSpaceId, api)
	}

	u := url.URL{
		Scheme: "https",
		Host:   app.Domain,
		Path:   path,
	}
	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if app.basicAuth {
		req.SetBasicAuth(app.basicAuthUser, app.basicAuthPassword)
	}
	if len(app.ApiToken) == 0 {
		req.Header.Set("X-Cybozu-Authorization", app.token)
	} else {
		req.Header.Set("X-Cybozu-API-Token", app.ApiToken)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", app.GetUserAgentHeader())

	return req, nil
}

func (app *App) do(req *http.Request) (*http.Response, error) {
	if app.Client == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, err
		}
		app.Client = &http.Client{Jar: jar}
	}
	if app.Timeout == time.Duration(0) {
		app.Timeout = DEFAULT_TIMEOUT
	}

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := app.Client.Do(req)
		done <- result{resp, err}
	}()

	type requestCanceler interface {
		CancelRequest(*http.Request)
	}

	select {
	case r := <-done:
		return r.resp, r.err
	case <-time.After(app.Timeout):
		if canceller, ok := app.Client.Transport.(requestCanceler); ok {
			canceller.CancelRequest(req)
		} else {
			go func() {
				r := <-done
				if r.err == nil {
					r.resp.Body.Close()
				}
			}()
		}
		return nil, ErrTimeout
	}
}

func isJSON(contentType string) bool {
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	return mediatype == "application/json"
}

func parseResponse(resp *http.Response) ([]byte, error) {
	body, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if !isJSON(resp.Header.Get("Content-Type")) {
			return nil, &AppError{
				HttpStatus:     resp.Status,
				HttpStatusCode: resp.StatusCode,
			}
		}

		// Get other than the Errors property
		var ae AppError
		json.Unmarshal(body, &ae)
		ae.HttpStatus = resp.Status
		ae.HttpStatusCode = resp.StatusCode

		// Get the Errors property
		var errors interface{}
		json.Unmarshal(body, &errors)
		msg := errors.(map[string]interface{})
		v, ok := msg["errors"]
		// If the Errors property exists
		if ok {
			result, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			ae.Errors = string(result)
		}
		return nil, &ae
	}
	return body, nil
}

// GetRecord fetches a record.
func (app *App) GetRecord(id uint64) (*Record, error) {
	type request_body struct {
		App uint64 `json:"app,string"`
		Id  uint64 `json:"id,string"`
	}
	data, _ := json.Marshal(request_body{app.AppId, id})
	req, err := app.newRequest("GET", "record", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	body, err := parseResponse(resp)
	if err != nil {
		return nil, err
	}
	rec, err := DecodeRecord(body)
	if err != nil {
		return nil, ErrInvalidResponse
	}
	return rec, nil
}

// GetRecords fetches records matching given conditions.
//
// This method can retrieve up to 100 records at once.
// To retrieve more records, you need to call GetRecords with
// increasing "offset" query parameter until the number of records
// retrieved becomes less than 100.
//
// If fields is nil, all fields are retrieved.
// See API specs how to construct query strings.
func (app *App) GetRecords(fields []string, query string) ([]*Record, error) {
	type request_body struct {
		App    uint64   `json:"app,string"`
		Fields []string `json:"fields"`
		Query  string   `json:"query"`
	}
	data, _ := json.Marshal(request_body{app.AppId, fields, query})
	req, err := app.newRequest("GET", "records", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	body, err := parseResponse(resp)
	if err != nil {
		return nil, err
	}
	recs, err := DecodeRecords(body)
	if err != nil {
		return nil, ErrInvalidResponse
	}
	return recs, nil
}

// GetAllRecords fetches all records.
//
// If fields is nil, all fields are retrieved.
func (app *App) GetAllRecords(fields []string) ([]*Record, error) {
	recs := make([]*Record, 0, 100)
	type request_body struct {
		App    uint64   `json:"app,string"`
		Fields []string `json:"fields"`
		Query  string   `json:"query"`
	}
	for {
		query := "limit 100"
		if len(recs) > 0 {
			query = fmt.Sprintf("limit 100 offset %v", len(recs))
		}
		data, _ := json.Marshal(request_body{app.AppId, fields, query})
		req, err := app.newRequest("GET", "records", bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		resp, err := app.do(req)
		if err != nil {
			return nil, err
		}
		body, err := parseResponse(resp)
		if err != nil {
			return nil, err
		}
		r, err := DecodeRecords(body)
		if err != nil {
			return nil, ErrInvalidResponse
		}
		recs = append(recs, r...)
		if len(r) < 100 {
			return recs, nil
		}
	}
}

func isAllowedLang(allowedLangs []string, lang string) bool {
	for _, allowedLang := range allowedLangs {
		if lang == allowedLang {
			return true
		}
	}
	return false
}

// GetProcess retrieves the process management settings
// This method can only be used when App is initialized with password authentication
// lang must be one of default, en, zh, ja, user
func (app *App) GetProcess(lang string) (process *Process, err error) {
	type request_body struct {
		App  uint64 `json:"app,string"`
		Lang string `json:"lang,string"`
	}
	if app.User == "" || app.Password == "" {
		err = errors.New("This API only supports password authentication")
		return
	}
	allowedLangs := []string{"default", "en", "zh", "ja", "user"}
	if !isAllowedLang(allowedLangs, lang) {
		err = errors.New("Illegal language provided")
		return
	}
	data, _ := json.Marshal(request_body{app.AppId, lang})
	req, err := app.newRequest("GET", "app/status", bytes.NewReader(data))
	if err != nil {
		return
	}
	resp, err := app.do(req)
	if err != nil {
		return
	}
	body, err := parseResponse(resp)
	if err != nil {
		return
	}
	process, err = DecodeProcess(body)
	if err != nil {
		err = ErrInvalidResponse
	}
	return
}

// FileData stores downloaded file data.
type FileData struct {
	ContentType string    // MIME type of the contents.
	Reader      io.Reader // File contents.
}

// Download fetches an attached file contents.
//
// fileKey should be obtained from FileField (= []File).
func (app *App) Download(fileKey string) (*FileData, error) {
	type request_body struct {
		FileKey string `json:"fileKey"`
	}
	data, _ := json.Marshal(request_body{fileKey})
	req, err := app.newRequest("GET", "file", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		if !isJSON(resp.Header.Get("Content-Type")) {
			return nil, &AppError{
				HttpStatus:     resp.Status,
				HttpStatusCode: resp.StatusCode,
			}
		}
		var ae AppError
		body, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		json.Unmarshal(body, &ae)
		ae.HttpStatus = resp.Status
		ae.HttpStatusCode = resp.StatusCode
		return nil, &ae
	}

	pin, pout := io.Pipe()
	go func() {
		_, err := io.Copy(pout, resp.Body)
		resp.Body.Close()
		if err != nil {
			pout.CloseWithError(err)
		} else {
			pout.Close()
		}
	}()
	return &FileData{resp.Header.Get("Content-Type"), pin}, nil
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string {
	return quoteEscaper.Replace(s)
}

// Upload uploads a file.
//
// If successfully uploaded, the key string of the uploaded file is returned.
func (app *App) Upload(fileName, contentType string, data io.Reader) (key string, err error) {
	f, err := ioutil.TempFile("", "go-kintone-")
	if err != nil {
		return
	}
	defer func(fn string) {
		_ = os.Remove(fn)
	}(f.Name())

	w := multipart.NewWriter(f)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename="%s"`,
			escapeQuotes(fileName)))
	h.Set("Content-Type", contentType)
	fw, err := w.CreatePart(h)
	if _, err = io.Copy(fw, data); err != nil {
		return
	}
	if err = w.Close(); err != nil {
		return
	}
	if _, err = f.Seek(0, 0); err != nil {
		return
	}

	req, err := app.newRequest("POST", "file", f)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := app.do(req)
	if err != nil {
		return
	}
	body, err := parseResponse(resp)
	if err != nil {
		return
	}

	var t struct {
		FileKey string `json:"fileKey"`
	}
	if json.Unmarshal(body, &t) != nil {
		err = ErrInvalidResponse
		return
	}
	return t.FileKey, nil
}

// AddRecord adds a new record.
//
// If successful, the record ID of the new record is returned.
func (app *App) AddRecord(rec *Record) (id string, err error) {
	type request_body struct {
		App    uint64  `json:"app,string"`
		Record *Record `json:"record"`
	}
	data, _ := json.Marshal(request_body{app.AppId, rec})
	req, err := app.newRequest("POST", "record", bytes.NewReader(data))
	if err != nil {
		return
	}
	resp, err := app.do(req)
	if err != nil {
		return
	}
	body, err := parseResponse(resp)
	if err != nil {
		return
	}

	var t struct {
		Id string `json:"id"`
	}
	if json.Unmarshal(body, &t) != nil {
		err = ErrInvalidResponse
		return
	}
	id = t.Id
	return
}

// AddRecords adds new records.
//
// Up to 100 records can be added at once.
// If successful, a list of record IDs is returned.
func (app *App) AddRecords(recs []*Record) ([]string, error) {
	if len(recs) > 100 {
		return nil, ErrTooMany
	}

	type request_body struct {
		App     uint64    `json:"app,string"`
		Records []*Record `json:"records"`
	}
	data, _ := json.Marshal(request_body{app.AppId, recs})
	req, err := app.newRequest("POST", "records", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	body, err := parseResponse(resp)
	if err != nil {
		return nil, err
	}

	var t struct {
		Ids []string `json:"ids"`
	}
	if json.Unmarshal(body, &t) != nil {
		return nil, ErrInvalidResponse
	}
	return t.Ids, nil
}

// UpdateRecord edits a record.
//
// If ignoreRevision is true, the record will always be updated despite
// the revision number.  Else, the record may not be updated when the
// same record was updated by another client.
func (app *App) UpdateRecord(rec *Record, ignoreRevision bool) error {
	type request_body struct {
		App      uint64  `json:"app,string"`
		Id       uint64  `json:"id,string"`
		Revision int64   `json:"revision,string"`
		Record   *Record `json:"record"`
	}
	rev := rec.Revision()
	if ignoreRevision {
		rev = -1
	}
	data, _ := json.Marshal(request_body{app.AppId, rec.id, rev, rec})
	req, err := app.newRequest("PUT", "record", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := app.do(req)
	if err != nil {
		return err
	}
	_, err = parseResponse(resp)
	return err
}

// UpdateRecordByKey edits a record by specified key field.
func (app *App) UpdateRecordByKey(rec *Record, ignoreRevision bool, keyField string) error {
	type request_body struct {
		App       uint64    `json:"app,string"`
		UpdateKey UpdateKey `json:"updateKey"`
		Revision  int64     `json:"revision,string"`
		Record    *Record   `json:"record"`
	}
	rev := rec.Revision()
	if ignoreRevision {
		rev = -1
	}
	updateKey := rec.Fields[keyField]
	_rec := *rec
	_rec.Fields = make(map[string]interface{})
	for k, v := range rec.Fields {
		if k != keyField {
			_rec.Fields[k] = v
		}
	}
	data, _ := json.Marshal(request_body{app.AppId, UpdateKey{keyField, updateKey.(UpdateKeyField)}, rev, &_rec})

	req, err := app.newRequest("PUT", "record", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := app.do(req)
	if err != nil {
		return err
	}
	_, err = parseResponse(resp)
	return err
}

// UpdateRecords edits multiple records at once.
//
// Up to 100 records can be edited at once.  ignoreRevision works the
// same as UpdateRecord method.
func (app *App) UpdateRecords(recs []*Record, ignoreRevision bool) error {
	if len(recs) > 100 {
		return ErrTooMany
	}

	type update_t struct {
		Id       uint64  `json:"id,string"`
		Revision int64   `json:"revision,string"`
		Record   *Record `json:"record"`
	}
	type request_body struct {
		App     uint64     `json:"app,string"`
		Records []update_t `json:"records"`
	}
	t_recs := make([]update_t, 0, len(recs))
	for _, rec := range recs {
		rev := rec.Revision()
		if ignoreRevision {
			rev = -1
		}
		t_recs = append(t_recs, update_t{rec.Id(), rev, rec})
	}
	data, _ := json.Marshal(request_body{app.AppId, t_recs})
	req, err := app.newRequest("PUT", "records", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := app.do(req)
	if err != nil {
		return err
	}
	_, err = parseResponse(resp)
	return err
}

// UpdateRecordsByKey edits multiple records by specified key fields at once.
func (app *App) UpdateRecordsByKey(recs []*Record, ignoreRevision bool, keyField string) error {
	if len(recs) > 100 {
		return ErrTooMany
	}

	type update_t struct {
		UpdateKey UpdateKey `json:"updateKey"`
		Revision  int64     `json:"revision,string"`
		Record    *Record   `json:"record"`
	}
	type request_body struct {
		App     uint64     `json:"app,string"`
		Records []update_t `json:"records"`
	}
	t_recs := make([]update_t, 0, len(recs))
	for _, rec := range recs {
		rev := rec.Revision()
		if ignoreRevision {
			rev = -1
		}
		updateKey := rec.Fields[keyField]
		_rec := *rec
		_rec.Fields = make(map[string]interface{})
		for k, v := range rec.Fields {
			if k != keyField {
				_rec.Fields[k] = v
			}
		}
		t_recs = append(t_recs, update_t{UpdateKey{keyField, updateKey.(UpdateKeyField)}, rev, &_rec})
	}
	data, _ := json.Marshal(request_body{app.AppId, t_recs})
	req, err := app.newRequest("PUT", "records", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := app.do(req)
	if err != nil {
		return err
	}
	_, err = parseResponse(resp)
	return err
}

// UpdateRecordStatus updates the Status of a record
func (app *App) UpdateRecordStatus(rec *Record, action *ProcessAction, assignee *Entity, ignoreRevision bool) (err error) {
	type request_body struct {
		App      uint64 `json:"app,string"`
		Id       uint64 `json:"id,string"`
		Revision int64  `json:"revision,string"`
		Action   string `json:"action"`
		Assignee string `json:"assignee,omitempty"`
	}
	rev := rec.Revision()
	if ignoreRevision {
		rev = -1
	}
	var code string
	if assignee != nil {
		code = assignee.Code
	}
	data, _ := json.Marshal(request_body{app.AppId, rec.id, rev, action.Name, code})
	req, err := app.newRequest("PUT", "record/status", bytes.NewReader(data))
	if err != nil {
		return
	}
	resp, err := app.do(req)
	if err != nil {
		return
	}
	_, err = parseResponse(resp)
	return
}

// DeleteRecords deletes multiple records.
//
// Up to 100 records can be deleted at once.
func (app *App) DeleteRecords(ids []uint64) error {
	if len(ids) > 100 {
		return ErrTooMany
	}

	type request_body struct {
		App uint64   `json:"app,string"`
		Ids []uint64 `json:"ids,string"`
	}
	data, _ := json.Marshal(request_body{app.AppId, ids})
	req, err := app.newRequest("DELETE", "records", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := app.do(req)
	if err != nil {
		return err
	}
	_, err = parseResponse(resp)
	return err
}

// GetRecordComments get comment list by record ID.
//
// It returns comment array.
func (app *App) GetRecordComments(recordID uint64, order string, offset, limit uint64) ([]Comment, error) {
	type requestBody struct {
		App    uint64 `json:"app"`
		Record uint64 `json:"record"`
		Order  string `json:"order"`
		Offset uint64 `json:"offset"`
		Limit  uint64 `json:"limit"`
	}

	data, _ := json.Marshal(requestBody{app.AppId, recordID, order, offset, limit})
	req, err := app.newRequest("GET", "record/comments", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	body, err := parseResponse(resp)
	if err != nil {
		return nil, ErrInvalidResponse
	}
	recs, err := DecodeRecordComments(body)
	if err != nil {
		return nil, err
	}

	return recs, nil
}

// AddRecordComment post some comments by record ID.
//
// If successful, it returns the target record ID.
func (app *App) AddRecordComment(recordId uint64, comment *Comment) (id string, err error) {
	type requestBody struct {
		App     uint64   `json:"app,string"`
		Record  uint64   `json:"record,string"`
		Comment *Comment `json:"comment"`
	}
	data, _ := json.Marshal(requestBody{app.AppId, recordId, comment})
	req, err := app.newRequest("POST", "record/comment", bytes.NewReader(data))
	if err != nil {
		return
	}
	resp, err := app.do(req)
	if err != nil {
		return
	}
	body, err := parseResponse(resp)
	if err != nil {
		return
	}
	var t struct {
		Id string `json:"id"`
	}
	if json.Unmarshal(body, &t) != nil {
		err = ErrInvalidResponse
		return
	}
	id = t.Id
	return
}

// DeleteComment - Delete single comment
func (app *App) DeleteComment(recordId uint64, commentId uint64) error {
	type requestBody struct {
		App       uint64 `json:"app,string"`
		RecordID  uint64 `json:"record,string"`
		CommentID uint64 `json:"comment,string"`
	}
	requestData := requestBody{app.AppId, recordId, commentId}
	data, _ := json.Marshal(requestData)

	req, err := app.newRequest("DELETE", "record/comment", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp, err := app.do(req)
	if err != nil {
		return err
	}
	_, err = parseResponse(resp)
	return err
}

// FieldInfo is the meta data structure of a field.
type FieldInfo struct {
	Label       string      `json:"label"`             // Label string
	Code        string      `json:"code"`              // Unique field code
	Type        string      `json:"type"`              // Field type.  One of FT_* constant.
	NoLabel     bool        `json:"noLabel"`           // true to hide the label
	Required    bool        `json:"required"`          // true if this field must be filled
	Unique      bool        `json:"unique"`            // true if field values must be unique
	MaxValue    interface{} `json:"maxValue"`          // nil or numeric string
	MinValue    interface{} `json:"minValue"`          // nil or numeric string
	MaxLength   interface{} `json:"maxLength"`         // nil or numeric string
	MinLength   interface{} `json:"minLength"`         // nil or numeric string
	Default     interface{} `json:"defaultValue"`      // anything
	DefaultTime interface{} `json:"defaultExpression"` // nil or "NOW"
	Options     []string    `json:"options"`           // list of selectable values
	Expression  string      `json:"expression"`        // to calculate values
	Separator   bool        `json:"digit"`             // true to use thousand separator
	Medium      string      `json:"protocol"`          // "WEB", "CALL", or "MAIL"
	Format      string      `json:"format"`            // "NUMBER", "NUMBER_DIGIT", "DATETIME", "DATE", "TIME", "HOUR_MINUTE", "DAY_HOUR_MINUTE"
	Fields      []FieldInfo `json:"fields"`            // Field list of this subtable
}

// Work around code to handle "true"/"false" strings as booleans...
func (fi *FieldInfo) UnmarshalJSON(data []byte) error {
	var t struct {
		Label       string      `json:"label"`
		Code        string      `json:"code"`
		Type        string      `json:"type"`
		NoLabel     string      `json:"noLabel"`
		Required    string      `json:"required"`
		Unique      string      `json:"unique"`
		MaxValue    interface{} `json:"maxValue"`
		MinValue    interface{} `json:"minValue"`
		MaxLength   interface{} `json:"maxLength"`
		MinLength   interface{} `json:"minLength"`
		Default     interface{} `json:"defaultValue"`
		DefaultTime interface{} `json:"defaultExpression"`
		Options     []string    `json:"options"`
		Expression  string      `json:"expression"`
		Separator   string      `json:"digit"`
		Medium      string      `json:"protocol"`
		Format      string      `json:"format"`
		Fields      []FieldInfo `json:"fields"`
	}
	err := json.Unmarshal(data, &t)
	if err != nil {
		return err
	}
	*fi = FieldInfo{
		t.Label, t.Code, t.Type,
		(t.NoLabel == "true"),
		(t.Required == "true"),
		(t.Unique == "true"),
		t.MaxValue, t.MinValue, t.MaxLength, t.MinLength,
		t.Default, t.DefaultTime, t.Options, t.Expression,
		(t.Separator == "true"),
		t.Medium, t.Format, t.Fields,
	}
	return nil
}

// Decode JSON from app/form/fields.json
func decodeFieldInfo(t AppFormFields, ret map[string]*FieldInfo) {
	itemsMap :=  t.Properties.(map[string]interface{})
	for k, v := range itemsMap {
		fi := FieldInfo{}
		for l, w := range v.(map[string]interface{}) {
			switch l {
			case "label":
				fi.Label = w.(string)
			case "code":
				fi.Code = w.(string)
			case "type":
				fi.Type = w.(string)
			case "noLabel":
				fi.NoLabel = w.(bool)
			case "required":
				fi.Required = w.(bool)
			case "unique":
				fi.Unique = w.(bool)
			case "maxValue":
				fi.MaxValue = w
			case "minValue":
				fi.MaxValue = w
			case "maxLength":
				fi.MaxLength = w
			case "minLength":
				fi.MinLength = w
			case "defaultValue":
				fi.Default = w
			case "defaultNowValue":
				fi.DefaultTime = w
			case "options":
				var sa []string
				for _, x := range w.(map[string]interface{}) {
					sa = append(sa, x.(map[string]interface{})["label"].(string))
				}
				fi.Options = sa
			case "expression":
				fi.Expression = w.(string)
			case "digit":
				fi.Separator = w.(bool)
			case "protocol":
				fi.Medium = w.(string)
			case "format":
				fi.Format = w.(string)
			case "fields":
				ret := make(map[string]*FieldInfo)
				var y AppFormFields
				y.Properties = w
			  decodeFieldInfo(y, ret)
        var sb []FieldInfo
				for z, _ := range ret {
        	sb = append(sb, *ret[z])
        }
        fi.Fields = sb
			default: break;
			}
		}
		switch fi.Type {
		case "GROUP":
			// Do not add to []FieldInfo
		case "REFERENCE_TABLE":
			// Do not add to []FieldInfo
		default:
			ret[k] = &fi
		}
	}
}

// Fields returns the meta data of the fields in this application.
//
// If successful, a mapping between field codes and FieldInfo is returned.
func (app *App) Fields() (map[string]*FieldInfo, error) {
	type request_body struct {
		App uint64 `json:"app,string"`
	}
	data, _ := json.Marshal(request_body{app.AppId})
	req, err := app.newRequest("GET", "app/form/fields", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	resp, err := app.do(req)
	if err != nil {
		return nil, err
	}
	body, err := parseResponse(resp)
	if err != nil {
		return nil, err
	}

	var t AppFormFields
	err = json.Unmarshal(body, &t)
	if err != nil {
		return nil, ErrInvalidResponse
	}

	ret := make(map[string]*FieldInfo)
	decodeFieldInfo(t, ret)
	return ret, nil
}

// CreateCursor return the meta data of the Cursor in this application
func (app *App) CreateCursor(fields []string, query string, size uint64) (*Cursor, error) {
	type cursor struct {
		App    uint64   `json:"app"`
		Fields []string `json:"fields"`
		Size   uint64   `json:"size"`
		Query  string   `json:"query"`
	}
	data := cursor{App: app.AppId, Fields: fields, Size: size, Query: query}
	jsonData, _ := json.Marshal(data)
	url := app.createUrl("records/cursor", "")
	request, err := app.NewRequest("POST", url.String(), bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	response, err := app.do(request)
	if err != nil {
		return nil, err
	}
	body, err := parseResponse(response)
	if err != nil {
		return nil, err
	}
	result, err := decodeCursor(body)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteCursor - Delete cursor by id
func (app *App) DeleteCursor(id string) error {
	type requestBody struct {
		Id string `json:"id"`
	}
	data, err := json.Marshal(requestBody{Id: id})
	if err != nil {
		return err
	}

	url := app.createUrl("records/cursor", "")
	request, err := app.NewRequest("DELETE", url.String(), bytes.NewBuffer(data))
	if err != nil {
		return err
	}

	response, err := app.do(request)
	if err != nil {
		return err
	}

	_, err = parseResponse(response)
	if err != nil {
		return err
	}

	return nil
}

// Using Cursor Id to get all records
// GetRecordsByCursor return the meta data of the Record in this application
func (app *App) GetRecordsByCursor(id string) (*GetRecordsCursorResponse, error) {
	url := app.createUrl("records/cursor", "id="+id)
	request, err := app.NewRequest("GET", url.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := app.do(request)
	if err != nil {
		return nil, err
	}
	data, err := parseResponse(response)
	if err != nil {
		return nil, err
	}
	recordsCursorResponse, err := DecodeGetRecordsCursorResponse(data)
	if err != nil {
		return nil, err
	}
	return recordsCursorResponse, nil
}
