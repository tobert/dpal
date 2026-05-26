package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	infoURI         = "dpal://info"
	sessionsURI     = "dpal://sessions"
	sessionTemplate = "dpal://session/{id}"
	sessionPrefix   = "dpal://session/"
	jsonMIME        = "application/json"
)

// InfoPayload is the JSON body served at dpal://info.
type InfoPayload struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	DefaultModel   string `json:"default_model"`
	SessionCap     int    `json:"session_cap"`
	ActiveSessions int    `json:"active_sessions"`
}

// SessionsPayload is the JSON body served at dpal://sessions.
type SessionsPayload struct {
	Count    int           `json:"count"`
	Cap      int           `json:"cap"`
	Sessions []SessionInfo `json:"sessions"`
}

// SessionPayload is the JSON body served at dpal://session/{id}.
type SessionPayload struct {
	ID       string              `json:"id"`
	Messages []TranscriptMessage `json:"messages"`
}

func (s *Server) infoJSON() ([]byte, error) {
	return json.MarshalIndent(InfoPayload{
		Name:           "dpal",
		Version:        s.version,
		DefaultModel:   s.defaultModel,
		SessionCap:     s.sessions.maxSize,
		ActiveSessions: s.sessions.Len(),
	}, "", "  ")
}

func (s *Server) sessionsJSON() ([]byte, error) {
	snap := s.sessions.Snapshot()
	return json.MarshalIndent(SessionsPayload{
		Count:    len(snap),
		Cap:      s.sessions.maxSize,
		Sessions: snap,
	}, "", "  ")
}

func (s *Server) sessionJSON(id string) ([]byte, error) {
	msgs, ok := s.sessions.Transcript(id)
	if !ok {
		return nil, fmt.Errorf("no session with id %q", id)
	}
	return json.MarshalIndent(SessionPayload{
		ID:       id,
		Messages: msgs,
	}, "", "  ")
}

func (s *Server) handleInfo(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	body, err := s.infoJSON()
	if err != nil {
		return nil, err
	}
	return jsonResource(infoURI, body), nil
}

func (s *Server) handleSessions(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	body, err := s.sessionsJSON()
	if err != nil {
		return nil, err
	}
	return jsonResource(sessionsURI, body), nil
}

func (s *Server) handleSession(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	id, err := parseSessionURI(req.Params.URI)
	if err != nil {
		return nil, err
	}
	body, err := s.sessionJSON(id)
	if err != nil {
		return nil, err
	}
	return jsonResource(req.Params.URI, body), nil
}

func parseSessionURI(uri string) (string, error) {
	id := strings.TrimPrefix(uri, sessionPrefix)
	if id == uri || id == "" {
		return "", fmt.Errorf("invalid session URI: %q (expected %s<id>)", uri, sessionPrefix)
	}
	return id, nil
}

func jsonResource(uri string, body []byte) *mcp.ReadResourceResult {
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: jsonMIME,
			Text:     string(body),
		}},
	}
}
