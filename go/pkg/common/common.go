package common

import (
	"fmt"
	"net/url"

	"github.com/jonasohland/mxl-fabrics-proxy/go/pkg/server"
)

type FlowTags struct {
	GroupHint []string `json:"urn:x-nmos:tag:grouphint/v1.0"`
}

type Rational struct {
	Numerator   int `json:"numerator"`
	Denominator int `json:"denominator"`
}

type Component struct {
	Name     string `json:"name"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	BitDepth int    `json:"bit_depth"`
}

type FlowDefinition struct {
	Copyright     string      `json:"$copyright"`
	License       string      `json:"$license"`
	Description   string      `json:"description"`
	SourceID      string      `json:"source_id,omitempty"`
	DeviceID      string      `json:"device_id,omitempty"`
	ID            string      `json:"id"`
	Tags          FlowTags    `json:"tags"`
	Format        string      `json:"format"`
	Label         string      `json:"label"`
	Version       string      `json:"version"`
	Parents       []string    `json:"parents"`
	MediaType     string      `json:"media_type"`
	GrainRate     *Rational   `json:"grain_rate,omitempty"`
	SampleRate    *Rational   `json:"sample_rate,omitempty"`
	FrameWidth    *int        `json:"frame_width,omitempty"`
	FrameHeight   *int        `json:"frame_height,omitempty"`
	InterlaceMode string      `json:"interlace_mode,omitempty"`
	Colorspace    string      `json:"colorspace,omitempty"`
	Componenets   []Component `json:"components,omitempty"`
	ChannelCount  *int        `json:"channel_count,omitempty"`
	BitDepth      *int        `json:"bit_depth,omitempty"`
}

type FlowInfo struct {
	FlowDefinition FlowDefinition `json:"flow_def.json"`
}

type DomainInfo struct {
	Path  string              `json:"path"`
	Name  string              `json:"name"`
	Node  string              `json:"node"`
	Flows map[string]FlowInfo `json:"flows"`
}

type SubscriptionRequest struct {
	FlowURL    string `json:"flow_url"`
	TargetInfo string `json:"target_info"`
	Provider   string `json:"provider"`
}

type SubscriptionResponse struct {
	ID string `json:"id"`
}

func ParseFlowURL(flowURL string) (string, []string, error) {
	furl, err := url.Parse(flowURL)
	if err != nil {
		return "", nil, fmt.Errorf("%w: parse flow url:  %w", server.ErrInvalidArgument, err)
	}

	return furl.Path, furl.Query()["id"], nil
}

func ParseFlowURLSingle(flowURL string) (string, string, error) {
	path, ids, err := ParseFlowURL(flowURL)
	if err != nil {
		return "", "", err
	}

	if len(ids) != 1 {
		return "", "", fmt.Errorf("%w: url must contain a single flow ID", server.ErrInvalidArgument)
	}

	return path, ids[0], nil
}

func ParseDomainURL(domainURL string) (string, error) {
	path, _, err := ParseFlowURL(domainURL)
	if err != nil {
		return "", err
	}

	return path, nil
}
