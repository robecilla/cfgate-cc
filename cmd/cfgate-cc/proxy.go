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
	//   - upstream: bytes to send to the upstream provider (already
	//     shaped for the upstream wire format)
	//   - model: the logical model id, post-modelID() mapping. used by
	//     response shapers so they emit a non-empty `model` field. NOT
	//     the post-cloudflare-rewrite wire model.
	//   - wireModel: the model id to put on the wire (post-cloudflare
	//     rewrite). may be empty for non-cloudflare configs. used only
	//     for applyCloudflareGatewayHeader.
	//   - stream: whether the client asked for a streaming response
	//   - includeUsage: whether the client asked for stream-usage
	//     chunks (OAI stream_options.include_usage). only meaningful
	//     for endpoints whose anthropic-source stream shaper honors it.
	//   - useAnthropic: true if Parse built an anthropic-shape body and
	//     intends the anthropic forward path. Parse is the only place
	//     that knows the dispatch — the scaffold just trusts it.
	//
	// Parse absorbs each endpoint's quirks (chat sanitization, responses
	// shape conversion, etc) and validation. on error, proxy.Do returns
	// 400 with err.Error().
	Parse func(rawBody []byte, cfg ProviderConfig) (upstream []byte, model string, wireModel string, stream bool, includeUsage bool, useAnthropic bool, err error)

	// Anthropic-source shapers: render the response when Parse said
	// useAnthropic. nil for an endpoint that never uses this branch.
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
	Parse: func(rawBody []byte, cfg ProviderConfig) ([]byte, string, string, bool, bool, bool, error) {
		var ar AnthropicRequest
		if err := json.Unmarshal(rawBody, &ar); err != nil {
			return nil, "", "", false, false, false, err
		}
		ar.Model = modelID(ar.Model)
		if modelUsesAnthropicEndpoint(ar.Model, cfg) {
			// ponytail: ensureAnthropicRequestDefaults only fires on
			// the anthropic branch in the original handler. kept here
			// to match.
			ensureAnthropicRequestDefaults(&ar, cfg)
			body, _ := json.Marshal(ar)
			return body, ar.Model, "", ar.Stream, false, true, nil
		}
		or := convertRequest(ar, cfg)
		if err := validateImageSupport(or); err != nil {
			return nil, "", "", false, false, false, err
		}
		body, _ := json.Marshal(or)
		body, wireModel := cloudflarePrepareBody(body, cfg)
		return body, or.Model, wireModel, ar.Stream, false, false, nil
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
	Parse: func(rawBody []byte, cfg ProviderConfig) ([]byte, string, string, bool, bool, bool, error) {
		body, err := prepareChatBody(rawBody, cfg)
		if err != nil {
			return nil, "", "", false, false, false, err
		}
		var or OAIRequest
		if err := json.Unmarshal(body, &or); err != nil {
			return nil, "", "", false, false, false, err
		}
		if modelUsesAnthropicEndpoint(or.Model, cfg) {
			or.Model = modelID(or.Model)
			if err := validateImageSupport(or); err != nil {
				return nil, "", "", false, false, false, err
			}
			upstream, _ := json.Marshal(chatToAnthropic(or, cfg))
			upstream, wireModel := cloudflarePrepareBody(upstream, cfg)
			includeUsage := or.StreamOptions != nil && or.StreamOptions.IncludeUsage
			return upstream, or.Model, wireModel, or.Stream, includeUsage, true, nil
		}
		body, wireModel := cloudflarePrepareBody(body, cfg)
		return body, or.Model, wireModel, or.Stream, false, false, nil
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
	Parse: func(rawBody []byte, cfg ProviderConfig) ([]byte, string, string, bool, bool, bool, error) {
		var rr ResponsesRequest
		if err := json.Unmarshal(rawBody, &rr); err != nil {
			return nil, "", "", false, false, false, err
		}
		or := responsesToChat(rr, cfg)
		if err := validateImageSupport(or); err != nil {
			return nil, "", "", false, false, false, err
		}
		if modelUsesAnthropicEndpoint(or.Model, cfg) {
			or.Model = modelID(or.Model)
			upstream, _ := json.Marshal(chatToAnthropic(or, cfg))
			upstream, wireModel := cloudflarePrepareBody(upstream, cfg)
			return upstream, or.Model, wireModel, rr.Stream, false, true, nil
		}
		body, _ := json.Marshal(or)
		body, wireModel := cloudflarePrepareBody(body, cfg)
		return body, or.Model, wireModel, rr.Stream, false, false, nil
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
// ponytail: the per-endpoint Parse function owns the anthropic-vs-oai
// dispatch — it knows the inbound shape, so the scaffold can't second-
// guess it. Parse signals the choice via useAnthropic. upgrade path:
// if a future endpoint needs per-endpoint shape rules beyond Parse
// (e.g. a separate "always anthropic regardless of model" path), add
// a per-endpoint function field on Endpoint.
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

	upstream, model, wireModel, stream, includeUsage, useAnthropic, err := ep.Parse(rawBody, cfg)
	if err != nil {
		dlogHandlerErr(ep.Label, err, http.StatusBadRequest)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

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
				ep.StreamAnthropic(w, sr, model, includeUsage)
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
			ep.StaticAnthropic(w, bytes.NewReader(bodyBytes), model)
		} else {
			copyHeaders(w.Header(), resp.Header)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(bodyBytes)
		}
		dlogClientResp(ep.Label, http.StatusOK)
		return
	}

	upstreamURL := openAIURLForModel(cfg, model)
	if isResponsesUpstreamURL(upstreamURL) {
		upstream = oaiBodyToResponsesBody(upstream)
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(upstream))
	if err != nil {
		dlogHandlerErr(ep.Label, err, http.StatusInternalServerError)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	applyUpstreamAuthForModel(req, cfg, wireModel)
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
	// ponytail: on the 4xx path we copy the upstream body verbatim. an
	// openai /v1/responses 400 has the same JSON error shape as
	// /v1/chat/completions, so no translation is needed there. skip the
	// responses→chat pass on errors and pass the bytes through.
	responsesUpstream := isResponsesUpstreamURL(upstreamURL)
	if resp.StatusCode >= 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		dlogBody("upstream", bodyBytes)
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		dlogClientResp(ep.Label, resp.StatusCode)
		_, _ = w.Write(bodyBytes)
		return
	}
	if stream {
		var r io.Reader = resp.Body
		if responsesUpstream {
			r = newResponsesStreamTranslator(r)
		}
		sr := &streamReader{r: r, label: ep.Label, start: time.Now()}
		if ep.StreamOAI != nil {
			ep.StreamOAI(w, sr, model)
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
	if responsesUpstream {
		converted, _ := responsesBodyToOAIChat(bodyBytes, model)
		bodyBytes = converted
	}
	if ep.StaticOAI != nil {
		ep.StaticOAI(w, bytes.NewReader(bodyBytes), model)
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
