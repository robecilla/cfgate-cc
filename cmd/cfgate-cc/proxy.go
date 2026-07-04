package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// proxy owns the common scaffold for the three inbound endpoints
// (/v1/messages, /v1/chat/completions, /v1/responses). the mux wires each
// path to proxy.Do with an Endpoint value that names the request parser and
// the response shapers.
//
// ponytail: Endpoint is a struct of funcs, not a Go interface — one struct,
// three concrete values. promote to interface if a fourth endpoint lands and
// the shape needs to vary at runtime.
type Endpoint struct {
	Label string

	// Parse decodes the inbound body. returns:
	//   - upstream: bytes to send to the upstream provider
	//   - wireModel: the model id to put on the wire (post-mapping)
	//   - stream: whether the client asked for a streaming response
	//   - includeUsage: whether the client asked for stream-usage
	//     chunks (OAI stream_options.include_usage). only meaningful
	//     for endpoints whose anthropic-source stream shaper honors it.
	//
	// Parse absorbs each endpoint's quirks (chat sanitization, responses
	// shape conversion, etc) and validation. on error, proxy.Do returns
	// 400 with err.Error().
	Parse func(rawBody []byte, cfg ProviderConfig) (upstream []byte, wireModel string, stream bool, includeUsage bool, err error)

	// Anthropic-source shapers: render the response when we forwarded
	// through forwardAnthropic (an anthropic-shaped upstream body). nil
	// for an endpoint that never goes through this branch — proxy.Do
	// guards with modelUsesAnthropicEndpoint(wireModel, cfg), so the
	// nil-when-unused case is just defensive.
	StaticAnthropic func(w http.ResponseWriter, body io.Reader, model string)
	StreamAnthropic func(w http.ResponseWriter, body io.Reader, model string, includeUsage bool)

	// OAI-source shapers: render the response when we forwarded through
	// openAIURL (an openai-shaped upstream body). nil means "copy
	// headers + body verbatim" — the chat passthrough case.
	StaticOAI func(w http.ResponseWriter, body io.Reader, model string)
	StreamOAI func(w http.ResponseWriter, body io.Reader, model string)
}

// messagesEndpoint: /v1/messages. anthropic inbound → either anthropic
// forward (stream/status passthrough) or OAI upstream (shaped back to
// anthropic on the way out).
var messagesEndpoint = Endpoint{
	Label: "messages",
	Parse: func(rawBody []byte, cfg ProviderConfig) ([]byte, string, bool, bool, error) {
		var ar AnthropicRequest
		if err := json.Unmarshal(rawBody, &ar); err != nil {
			return nil, "", false, false, err
		}
		ar.Model = modelID(ar.Model)
		ensureAnthropicRequestDefaults(&ar, cfg)
		body, _ := json.Marshal(ar)
		body, wireModel := cloudflarePrepareBody(body, cfg)
		return body, wireModel, ar.Stream, false, nil
	},
	StaticAnthropic: nil, // messages OAI branch shapes back to anthropic — same path as StaticOAI.
	StreamAnthropic: nil,
	StaticOAI:       writeAnthropicResponse,
	StreamOAI:       streamAnthropic,
}

// chatEndpoint: /v1/chat/completions. OAI inbound → either anthropic
// forward (shaped from OAI) or OAI upstream (verbatim passthrough).
var chatEndpoint = Endpoint{
	Label: "chat",
	Parse: func(rawBody []byte, cfg ProviderConfig) ([]byte, string, bool, bool, error) {
		body, err := prepareChatBody(rawBody, cfg)
		if err != nil {
			return nil, "", false, false, err
		}
		var or OAIRequest
		if err := json.Unmarshal(body, &or); err != nil {
			return nil, "", false, false, err
		}
		if modelUsesAnthropicEndpoint(or.Model, cfg) {
			or.Model = modelID(or.Model)
			if err := validateImageSupport(or); err != nil {
				return nil, "", false, false, err
			}
			upstream, _ := json.Marshal(chatToAnthropic(or, cfg))
			upstream, wireModel := cloudflarePrepareBody(upstream, cfg)
			includeUsage := or.StreamOptions != nil && or.StreamOptions.IncludeUsage
			return upstream, wireModel, or.Stream, includeUsage, nil
		}
		body, wireModel := cloudflarePrepareBody(body, cfg)
		return body, wireModel, or.Stream, false, nil
	},
	StaticAnthropic: writeChatCompletionsResponseFromAnthropic,
	StreamAnthropic: streamChatCompletionsFromAnthropic,
	StaticOAI:       nil, // chat OAI passthrough: copy headers + body verbatim.
	StreamOAI:       nil,
}

// responsesEndpoint: /v1/responses. responses-shape inbound → either
// anthropic forward (shaped to OAI first) or OAI upstream (shaped to
// /v1/responses on the way out).
var responsesEndpoint = Endpoint{
	Label: "responses",
	Parse: func(rawBody []byte, cfg ProviderConfig) ([]byte, string, bool, bool, error) {
		var rr ResponsesRequest
		if err := json.Unmarshal(rawBody, &rr); err != nil {
			return nil, "", false, false, err
		}
		or := responsesToChat(rr, cfg)
		if err := validateImageSupport(or); err != nil {
			return nil, "", false, false, err
		}
		if modelUsesAnthropicEndpoint(or.Model, cfg) {
			or.Model = modelID(or.Model)
			upstream, _ := json.Marshal(chatToAnthropic(or, cfg))
			upstream, wireModel := cloudflarePrepareBody(upstream, cfg)
			return upstream, wireModel, rr.Stream, false, nil
		}
		body, _ := json.Marshal(or)
		body, wireModel := cloudflarePrepareBody(body, cfg)
		return body, wireModel, rr.Stream, false, nil
	},
	StaticAnthropic: writeResponsesResponseFromAnthropic,
	StreamAnthropic: streamResponsesFromAnthropicWrapper,
	StaticOAI:       writeResponsesResponse,
	StreamOAI:       streamResponses,
}

// proxy.Do is the single scaffold for all three inbound endpoints. the 7
// steps live here in one place; per-endpoint variation is the Endpoint
// value passed in.
//
// ponytail: per-model dispatch (modelUsesAnthropicEndpoint) is computed
// here, not on the Endpoint. upgrade path: per-endpoint dispatch as a
// function field on Endpoint if a future path needs per-endpoint shape
// rules.
func proxyDo(w http.ResponseWriter, r *http.Request, cfg ProviderConfig, ep Endpoint) {
	if r.Method != http.MethodPost {
		dlogHandlerErr(ep.Label, errors.New("method not allowed"), http.StatusMethodNotAllowed)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		dlogHandlerErr(ep.Label, err, http.StatusBadRequest)
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	dlogIncoming(ep.Label, r, rawBody)

	upstream, wireModel, stream, includeUsage, err := ep.Parse(rawBody, cfg)
	if err != nil {
		dlogHandlerErr(ep.Label, err, http.StatusBadRequest)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	useAnthropic := modelUsesAnthropicEndpoint(wireModel, cfg)
	if useAnthropic {
		var ar AnthropicRequest
		_ = json.Unmarshal(upstream, &ar) // upstream is anthropic-shape; best-effort for dlog
		resp, err := forwardAnthropic(r.Context(), cfg, ar)
		if err != nil {
			dlogHandlerErr(ep.Label, err, http.StatusBadGateway)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		dlogUpstreamResp(resp)
		if resp.StatusCode >= 400 {
			bodyBytes, _ := io.ReadAll(resp.Body)
			dlogBody("upstream", bodyBytes)
			w.WriteHeader(resp.StatusCode)
			dlogClientResp(ep.Label, resp.StatusCode)
			_, _ = w.Write(bodyBytes)
			return
		}
		if stream {
			if ep.StreamAnthropic != nil {
				sr := &streamReader{r: resp.Body, label: ep.Label, start: time.Now()}
				ep.StreamAnthropic(w, sr, wireModel, includeUsage)
			} else {
				// ponytail: should not happen — every endpoint with an
				// anthropic branch has a stream shaper. fall back to
				// raw passthrough rather than 500.
				copyHeaders(w.Header(), resp.Header)
				w.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(w, resp.Body)
			}
			dlogClientResp(ep.Label, http.StatusOK)
			return
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		dlogBody("upstream", bodyBytes)
		if ep.StaticAnthropic != nil {
			ep.StaticAnthropic(w, bytes.NewReader(bodyBytes), wireModel)
		} else {
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(bodyBytes)
		}
		dlogClientResp(ep.Label, http.StatusOK)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, openAIURL(cfg), bytes.NewReader(upstream))
	if err != nil {
		dlogHandlerErr(ep.Label, err, http.StatusInternalServerError)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	applyUpstreamAuth(req, cfg)
	applyCloudflareGatewayHeader(req, cfg, wireModel)
	req.Header.Set("Content-Type", "application/json")
	dlogUpstreamReq(req, upstream)
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Do(req)
	if err != nil {
		dlogHandlerErr(ep.Label, err, http.StatusBadGateway)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	dlogUpstreamResp(resp)
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		dlogBody("upstream", bodyBytes)
		w.WriteHeader(resp.StatusCode)
		dlogClientResp(ep.Label, resp.StatusCode)
		_, _ = w.Write(bodyBytes)
		return
	}
	if stream {
		sr := &streamReader{r: resp.Body, label: ep.Label, start: time.Now()}
		if ep.StreamOAI != nil {
			ep.StreamOAI(w, sr, wireModel)
		} else {
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, sr)
		}
		dlogClientResp(ep.Label, http.StatusOK)
		return
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	dlogBody("upstream", bodyBytes)
	if ep.StaticOAI != nil {
		ep.StaticOAI(w, bytes.NewReader(bodyBytes), wireModel)
	} else {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(bodyBytes)
	}
	dlogClientResp(ep.Label, http.StatusOK)
}

// streamResponsesFromAnthropicWrapper adapts the 3-arg
// streamResponsesFromAnthropic shaper to the 4-arg StreamAnthropic
// signature. includeUsage is ignored: the responses wire format has no
// OAI-style include_usage flag, so the shaper never honored it.
// ponytail: the divergence is real (chat honors includeUsage, responses
// doesn't) and the wrapper names it. alternative was two shaper fields
// on Endpoint — more type, same code.
func streamResponsesFromAnthropicWrapper(w http.ResponseWriter, body io.Reader, model string, _ bool) {
	streamResponsesFromAnthropic(w, body, model)
}
