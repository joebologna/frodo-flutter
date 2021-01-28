package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/robsignorelli/frodo/internal/reflection"
	"github.com/robsignorelli/frodo/rpc/metadata"
)

// NewClient constructs the RPC client that does the "heavy lifting" when communicating
// with remote frodo-powered RPC services. It contains all data/logic required to marshal/unmarshal
// requests/responses as well as communicate w/ the remote service.
func NewClient(name string, addr string, options ...ClientOption) Client {
	defaultTimeout := 30 * time.Second
	client := Client{
		HTTP: &http.Client{
			Timeout: defaultTimeout,
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: defaultTimeout}).DialContext,
				TLSHandshakeTimeout: defaultTimeout,
			},
		},
		Name:       name,
		BaseURL:    strings.TrimSuffix(addr, "/"),
		middleware: clientMiddlewarePipeline{},
	}
	for _, option := range options {
		option(&client)
	}

	mw := clientMiddlewarePipeline{
		writeMetadataHeader,
	}
	client.middleware = append(mw, client.middleware...)
	client.roundTrip = client.middleware.Then(client.HTTP.Do)

	return client
}

// WithHTTPClient allows you to provide an HTTP client configured to your liking. You do not *need*
// to supply this. The default client already implements a 30 second timeout, but if you want a
// different timeout or custom dialer/transport/etc, then you can feed in you custom client here and
// we'll use that one for all HTTP communication with other services.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(rpcClient *Client) {
		rpcClient.HTTP = httpClient
	}
}

// ClientOption is a single configurable setting that modifies some attribute of the RPC client
// when building one via NewClient().
type ClientOption func(*Client)

// Client manages all RPC communication with other frodo-powered services. It uses HTTP under the hood,
// so you can supply a custom HTTP client by including WithHTTPClient() when calling your client
// constructor, NewXxxServiceClient().
type Client struct {
	// HTTP takes care of the raw HTTP request/response logic used when communicating w/ remote services.
	HTTP *http.Client
	// BaseURL contains the protocol/host/port/etc that is the prefix for all service function
	// endpoints. (e.g. "http://api.myawesomeapp.com")
	BaseURL string
	// PathPrefix sits between the host/port and the endpoint path (e.g. something like "v2") so that
	// you can segment/version your services. Typically this will be the same as what you apply as
	// the gateway's path prefix.
	PathPrefix string
	// Name is just the display name of the service; used only for debugging/tracing purposes.
	Name string
	// Middleware defines all of the units of work we will apply to the request/response when
	// round-tripping our RPC call to he remote service.
	middleware clientMiddlewarePipeline
	// roundTrip captures all middleware and the actual request dispatching in a single handler
	// function. This is what we'll call once we've created the HTTP/RPC request when invoking
	// one of your client's service functions.
	roundTrip RoundTripperFunc
}

// Invoke handles the standard request/response logic used to call a service method on the remote service.
// You should NOT call this yourself. Instead, you should stick to the strongly typed, code-generated
// service functions on your client.
func (c Client) Invoke(ctx context.Context, method string, path string, serviceRequest interface{}, serviceResponse interface{}) error {
	// Step 1: Fill in the URL path and query string w/ fields from the request. (e.g. /user/:id -> /user/abc)
	address := c.buildURL(method, path, serviceRequest)
	log.Printf("Invoking %s -> %s %s", c.Name, method, address)

	// Step 2: Create a JSON reader for the request body (POST/PUT/PATCH only).
	body, err := c.createRequestBody(method, serviceRequest)
	if err != nil {
		return fmt.Errorf("rpc: unable to create request body: %w", err)
	}

	// Step 3: Form the HTTP request
	request, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return fmt.Errorf("rpc: unable to create request: %w", err)
	}

	// Step 4: Run the request through all middleware and fire it off.
	response, err := c.roundTrip(request)
	if err != nil {
		return fmt.Errorf("rpc: round trip error: %w", err)
	}
	defer response.Body.Close()

	// Step 5: Based on the status code, either fill in the "out" struct (service response) with the
	// unmarshaled JSON or respond a properly formed error.
	if response.StatusCode >= 400 {
		errData, _ := ioutil.ReadAll(response.Body)
		return fmt.Errorf("rpc: http status %v: %s", response.StatusCode, string(errData))
	}

	err = json.NewDecoder(response.Body).Decode(serviceResponse)
	if err != nil {
		return fmt.Errorf("rpc: unable to decode response: %w", err)
	}
	return nil
}

func (c Client) createRequestBody(method string, serviceRequest interface{}) (io.Reader, error) {
	if shouldEncodeUsingQueryString(method) {
		return nil, nil
	}
	body := &bytes.Buffer{}
	err := json.NewEncoder(body).Encode(serviceRequest)
	return body, err
}

func (c Client) buildURL(method string, path string, serviceRequest interface{}) string {
	attributes := reflection.StructToMap(serviceRequest)

	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")
	pathSegments := strings.Split(path, "/")

	for i, pathSegment := range pathSegments {
		if strings.HasPrefix(pathSegment, ":") {
			paramName := pathSegment[1:]
			pathSegments[i] = fmt.Sprintf("%v", attributes[paramName])
			delete(attributes, paramName) // so it doesn't also get encoded in the query string
		}
	}

	// If we're doing a POST/PUT/PATCH, don't bother adding query string arguments.
	address := c.BaseURL + toEndpointPath(c.PathPrefix, strings.Join(pathSegments, "/"))
	if shouldEncodeUsingBody(method) {
		return address
	}

	// We're doing a GET/DELETE/etc, so all request values must come via query string args
	queryString := url.Values{}
	for name, value := range attributes {
		queryString.Set(name, fmt.Sprintf("%v", value))
	}
	return address + "?" + queryString.Encode()
}

func shouldEncodeUsingBody(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
}

func shouldEncodeUsingQueryString(method string) bool {
	return !shouldEncodeUsingBody(method)
}

// writeMetadataHeader encodes all of the context's (the context on the request) metadata values as
// JSON and writes that to the "X-RPC-Values" header so that the remote service has access to all
// of your values as well.
func writeMetadataHeader(request *http.Request, next RoundTripperFunc) (*http.Response, error) {
	encodedValues, err := metadata.ToJSON(request.Context())
	if err != nil {
		return nil, err
	}
	request.Header.Set(metadata.RequestHeader, encodedValues)
	return next(request)
}
