package proxmox

// inspired by https://github.com/openstack/golang-client/blob/master/openstack/session.go

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
)

var Debug = new(bool)

type Response struct {
	Resp *http.Response
	Body []byte
}

type Session struct {
	httpClient *http.Client
	ApiUrl     string
	AuthTicket string
	CsrfToken  string
	AuthToken  string // Combination of user, realm, token ID and UUID
	Headers    http.Header
}

// secureTransport wraps an http.RoundTripper to validate headers before sending requests.
// While Go's default HTTP client already validates headers and prevents invalid characters,
// it returns error messages containing the full header value. For sensitive headers like
// authentication tokens, this could leak secrets into logs or error messages.
// This transport performs the validation earlier and returns sanitized errors.
type secureTransport struct {
	underlying http.RoundTripper
}

func (t *secureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := validateHeader(req.Header); err != nil {
		return nil, fmt.Errorf("invalid header: %w", err)
	}
	return t.underlying.RoundTrip(req)
}

// validateHeader checks for invalid characters in header values without exposing the values
// in error messages. This prevents sensitive data like authentication tokens from being
// leaked when they contain invalid characters.
// As per RFC 7230, control characters (ASCII codes 0-31) and DEL (127) are not allowed
// in HTTP header values.
func validateHeader(h http.Header) error {
	for key, values := range h {
		for _, v := range values {
			for _, r := range v {
				if r < 32 || r == 127 {
					return fmt.Errorf("value for key %q contains invalid characters", key)
				}
			}
		}
	}
	return nil
}

func NewSession(apiUrl string, hclient *http.Client, proxyString string, tls *tls.Config) (session *Session, err error) {
	if hclient == nil {
		var tr *http.Transport
		if proxyString == "" {
			tr = &http.Transport{
				TLSClientConfig:    tls,
				DisableCompression: true,
				Proxy:              nil,
			}
		} else {
			proxyURL, err := url.ParseRequestURI(proxyString)
			if err != nil {
				return nil, err
			}
			if _, _, err := net.SplitHostPort(proxyURL.Host); err != nil {
				return nil, err
			}
			tr = &http.Transport{
				TLSClientConfig:    tls,
				DisableCompression: true,
				Proxy:              http.ProxyURL(proxyURL),
			}
		}
		hclient = &http.Client{Transport: &secureTransport{underlying: tr}}
	}
	session = &Session{
		httpClient: hclient,
		ApiUrl:     apiUrl,
		AuthTicket: "",
		CsrfToken:  "",
		Headers:    http.Header{},
	}
	return session, nil
}

func ParamsToBody(params map[string]interface{}) (body []byte) {
	vals := ParamsToValuesWithEmpty(params, []string{})
	body = bytes.NewBufferString(vals.Encode()).Bytes()
	return
}

func ParamsToValues(params map[string]interface{}) (vals url.Values) {
	vals = ParamsToValuesWithEmpty(params, []string{})
	return
}

func ParamsToBodyWithEmpty(params map[string]interface{}, allowedEmpty []string) (body []byte) {
	vals := ParamsToValuesWithEmpty(params, allowedEmpty)
	body = bytes.NewBufferString(vals.Encode()).Bytes()
	return
}

func ParamsToBodyWithAllEmpty(params map[string]interface{}) (body []byte) {
	vals := ParamsToValuesWithAllEmpty(params, []string{}, true)
	body = bytes.NewBufferString(vals.Encode()).Bytes()
	return
}

func ParamsToValuesWithEmpty(params map[string]interface{}, allowedEmpty []string) (vals url.Values) {
	return ParamsToValuesWithAllEmpty(params, allowedEmpty, false)
}

func ParamsToValuesWithAllEmpty(params map[string]interface{}, allowedEmpty []string, allowEmpty bool) (vals url.Values) {
	vals = url.Values{}
	for k, intrV := range params {
		var v string
		switch intrV := intrV.(type) {
		// Convert true/false bool to 1/0 string where Proxmox API can understand it.
		case bool:
			if intrV {
				v = "1"
			} else {
				v = "0"
			}
		default:
			v = fmt.Sprintf("%v", intrV)
		}
		if allowEmpty {
			vals.Set(k, v)
		} else if v != "" || inArray(allowedEmpty, k) {
			vals.Set(k, v)
		}
	}
	return
}

func decodeResponse(resp *http.Response, v interface{}) error {
	if resp.Body == nil {
		return nil
	}
	rbody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("error reading response body: %s", err)
	}
	if err = json.Unmarshal(rbody, &v); err != nil {
		return err
	}
	return nil
}

func ResponseJSON(resp *http.Response) (jbody map[string]interface{}, err error) {
	err = decodeResponse(resp, &jbody)
	return jbody, err
}

func TypedResponse(resp *http.Response, v interface{}) error {
	var intermediate struct {
		Data struct {
			Result json.RawMessage `json:"result"`
		} `json:"data"`
	}
	err := decodeResponse(resp, &intermediate)
	if err != nil {
		return fmt.Errorf("error reading response envelope: %v", err)
	}
	if err = json.Unmarshal(intermediate.Data.Result, v); err != nil {
		return fmt.Errorf("error unmarshalling result %v", err)
	}
	return nil
}

func (s *Session) SetAPIToken(userID, token string) {
	auth := fmt.Sprintf("%s=%s", userID, token)
	s.AuthToken = auth
}

func (s *Session) Login(username string, password string, otp string) (err error) {
	reqUser := map[string]interface{}{"username": username, "password": password}
	if otp != "" {
		reqUser["otp"] = otp
	}
	reqbody := ParamsToBody(reqUser)
	olddebug := *Debug
	*Debug = false // don't share passwords in debug log
	resp, err := s.Post("/access/ticket", nil, &s.Headers, &reqbody)
	*Debug = olddebug
	if err != nil {
		return err
	}
	if resp == nil {
		return fmt.Errorf("Login error reading response")
	}
	dr, _ := httputil.DumpResponse(resp, true)
	jbody, err := ResponseJSON(resp)
	if err != nil {
		return err
	}
	if jbody == nil || jbody["data"] == nil {
		return fmt.Errorf("invalid login response:\n-----\n%s\n-----", dr)
	}
	dat := jbody["data"].(map[string]interface{})
	//Check if the 2FA was required
	if dat["NeedTFA"] == 1.0 {
		return fmt.Errorf("missing TFA code")
	}
	s.AuthTicket = dat["ticket"].(string)
	s.CsrfToken = dat["CSRFPreventionToken"].(string)
	return nil
}

func (s *Session) NewRequest(method, url string, headers *http.Header, body io.Reader) (req *http.Request, err error) {
	req, err = http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		req.Header = *headers
	}
	if s.AuthToken != "" {
		req.Header["Authorization"] = []string{"PVEAPIToken=" + s.AuthToken}
	} else if s.AuthTicket != "" {
		req.Header["Authorization"] = []string{"PVEAuthCookie=" + s.AuthTicket}
		req.Header["CSRFPreventionToken"] = []string{s.CsrfToken}
	}
	return
}

func (s *Session) Do(req *http.Request) (*http.Response, error) {
	// Add session headers
	for k, v := range s.Headers {
		req.Header[k] = v
	}

	if *Debug {
		d, _ := httputil.DumpRequestOut(req, true)
		log.Printf(">>>>>>>>>> REQUEST:\n%v", string(d))
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	// The response body reader needs to be closed, but lots of places call
	// session.Do, and they might not be able to reliably close it themselves.
	// Therefore, read the body out, close the original, then replace it with
	// a NopCloser over the bytes, which does not need to be closed downsteam.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	if *Debug {
		dr, _ := httputil.DumpResponse(resp, true)
		log.Printf("<<<<<<<<<< RESULT:\n%v", string(dr))
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp, fmt.Errorf(resp.Status)
	}

	return resp, nil
}

// Perform a simple get to an endpoint
func (s *Session) Request(
	method string,
	url string,
	params *url.Values,
	headers *http.Header,
	body *[]byte,
) (resp *http.Response, err error) {
	// add params to url here
	url = s.ApiUrl + url
	if params != nil {
		url = url + "?" + params.Encode()
	}

	// Get the body if one is present
	var buf io.Reader
	if body != nil {
		buf = bytes.NewReader(*body)
	}

	req, err := s.NewRequest(method, url, headers, buf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")

	return s.Do(req)
}

// Perform a simple get to an endpoint and unmarshal returned JSON
func (s *Session) RequestJSON(
	method string,
	url string,
	params *url.Values,
	headers *http.Header,
	body interface{},
	responseContainer interface{},
) (resp *http.Response, err error) {
	var bodyjson []byte
	if body != nil {
		bodyjson, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}

	// if headers == nil {
	// 	headers = &http.Header{}
	// 	headers.Add("Content-Type", "application/json")
	// }

	resp, err = s.Request(method, url, params, headers, &bodyjson)
	if err != nil {
		return resp, err
	}

	// err = util.CheckHTTPResponseStatusCode(resp)
	// if err != nil {
	// 	return nil, err
	// }

	rbody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, fmt.Errorf("error reading response body")
	}
	if err = json.Unmarshal(rbody, &responseContainer); err != nil {
		return resp, err
	}

	return resp, nil
}

func (s *Session) Delete(
	url string,
	params *url.Values,
	headers *http.Header,
) (resp *http.Response, err error) {
	return s.Request("DELETE", url, params, headers, nil)
}

func (s *Session) Get(
	url string,
	params *url.Values,
	headers *http.Header,
) (resp *http.Response, err error) {
	return s.Request("GET", url, params, headers, nil)
}

func (s *Session) GetJSON(
	url string,
	params *url.Values,
	headers *http.Header,
	responseContainer interface{},
) (resp *http.Response, err error) {
	return s.RequestJSON("GET", url, params, headers, nil, responseContainer)
}

func (s *Session) Head(
	url string,
	params *url.Values,
	headers *http.Header,
) (resp *http.Response, err error) {
	return s.Request("HEAD", url, params, headers, nil)
}

func (s *Session) Post(
	url string,
	params *url.Values,
	headers *http.Header,
	body *[]byte,
) (resp *http.Response, err error) {
	if headers == nil {
		headers = &http.Header{}
		headers.Add("Content-Type", "application/x-www-form-urlencoded")
	}
	return s.Request("POST", url, params, headers, body)
}

func (s *Session) PostJSON(
	url string,
	params *url.Values,
	headers *http.Header,
	body interface{},
	responseContainer interface{},
) (resp *http.Response, err error) {
	return s.RequestJSON("POST", url, params, headers, body, responseContainer)
}

func (s *Session) Put(
	url string,
	params *url.Values,
	headers *http.Header,
	body *[]byte,
) (resp *http.Response, err error) {
	if headers == nil {
		headers = &http.Header{}
		headers.Add("Content-Type", "application/x-www-form-urlencoded")
	}
	return s.Request("PUT", url, params, headers, body)
}
