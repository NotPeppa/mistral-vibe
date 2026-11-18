package main

import (
	"context"
	_ "embed"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed page.html
var pageHTML []byte

// pageHandler serves the plugin's own page: the plan's balance and the one
// button that adds Mistral to CPA's AI providers.
//
// Resource routes are not management-authenticated, so the page never receives
// a key or a management key. It reads balance through the authenticated
// management route with whatever credentials the panel injects, and posts the
// connect form there too.
type pageHandler struct{}

func (pageHandler) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead:
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       pageHTML,
		}, nil
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed}, nil
}
