package server

import (
	"bytes"
	"encoding/json"

	"github.com/1mb-dev/shim/internal/translate"
)

// explainResponse is the POST /v1/messages/explain payload: the request shim
// WOULD send upstream for a given inbound body, plus every mutation it would
// apply — computed WITHOUT calling the upstream. The tangible form of thesis-2
// (loud-fail on drift): see exactly what the proxy changes before trusting it.
//
// Transport reports what shim actually did to the bytes ("passthrough" =
// forwarded verbatim, "translated" = reshaped), derived from the bytes
// themselves (see buildExplain) rather than a static dialect label — so it
// reflects reality, including a future identity translator that did mutate.
type explainResponse struct {
	Adapter         string            `json:"adapter"`
	Transport       string            `json:"transport"`
	UpstreamRequest json.RawMessage   `json:"upstream_request"`
	Mutations       []explainMutation `json:"mutations"`
}

// explainMutation is one loud-fail transformation. From/To are typed per kind:
// strings for model_rewrite, ints for stop_sequences_capped.
type explainMutation struct {
	Type string `json:"type"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

const (
	transportPassthrough = "passthrough"
	transportTranslated  = "translated"
	mutationModelRewrite = "model_rewrite"
	mutationStopCapped   = "stop_sequences_capped"
)

// buildExplain assembles the explain payload. Mutations are gated on transport:
// a verbatim passthrough changed nothing by definition — so MapModel's output,
// which the identity translator ignores, is correctly NOT reported as a rewrite.
// A translated transport reports the model rewrite and stop-sequence cap that
// actually applied. inbound is the client's body; upstream is what ToUpstream
// produced (already known to be valid JSON — it round-tripped or was forwarded
// verbatim).
func buildExplain(adapterName string, req *translate.AnthropicRequest, inbound, upstream []byte, mappedModel string, stopCapped int) explainResponse {
	mutations := []explainMutation{}
	transport := transportTranslated
	if bytes.Equal(inbound, upstream) {
		transport = transportPassthrough
	} else {
		if req.Model != mappedModel {
			mutations = append(mutations, explainMutation{
				Type: mutationModelRewrite, From: req.Model, To: mappedModel,
			})
		}
		if stopCapped > 0 {
			mutations = append(mutations, explainMutation{
				Type: mutationStopCapped,
				From: len(req.StopSequences),
				To:   len(req.StopSequences) - stopCapped,
			})
		}
	}
	return explainResponse{
		Adapter:         adapterName,
		Transport:       transport,
		UpstreamRequest: json.RawMessage(upstream),
		Mutations:       mutations,
	}
}
