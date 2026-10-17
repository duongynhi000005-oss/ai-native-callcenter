// SPDX-License-Identifier: Apache-2.0

package voice

import "github.com/rasonyang/ai-native-callcenter/internal/media"

// This file exports the SIP and SDP building blocks of the UAS unchanged, for
// the test tools that play the other side of a call (internal/calleesim). Each
// function is a thin forward to the unexported one the UAS itself uses, so the
// tools and the application parse and answer SIP the same way. Nothing here
// is called by the application.

// SIPMessage is a parsed SIP request or response.
type SIPMessage struct{ msg *sipMessage }

// ParseSIP parses one datagram.
func ParseSIP(data []byte) (*SIPMessage, error) {
	msg, err := parseSIP(data)
	if err != nil {
		return nil, err
	}
	return &SIPMessage{msg: msg}, nil
}

// Method is the request method, empty for a response or an unsupported method.
func (m *SIPMessage) Method() string { return string(m.msg.method) }

// RequestURI is empty for a response.
func (m *SIPMessage) RequestURI() string { return m.msg.requestURI }

// StatusCode is zero for a request.
func (m *SIPMessage) StatusCode() int { return m.msg.statusCode }

// Header returns the first value of a header, looked up case-insensitively.
func (m *SIPMessage) Header(name string) string { return m.msg.get(name) }

// Headers returns every header's first value, keyed by lowercase name.
func (m *SIPMessage) Headers() map[string]string {
	out := make(map[string]string, len(m.msg.headers))
	for k, v := range m.msg.headers {
		out[k] = v
	}
	return out
}

func (m *SIPMessage) CallID() string  { return m.msg.callID() }
func (m *SIPMessage) From() string    { return m.msg.fromHeader() }
func (m *SIPMessage) To() string      { return m.msg.toHeader() }
func (m *SIPMessage) CSeq() string    { return m.msg.cseq() }
func (m *SIPMessage) Contact() string { return m.msg.contact() }
func (m *SIPMessage) Body() string    { return m.msg.body }

// RecordRoutes returns the Record-Route values in the order received.
func (m *SIPMessage) RecordRoutes() []string { return m.msg.recordRoutes() }

// Response answers the message; see buildResponse for the arguments.
func (m *SIPMessage) Response(status, toTag, cseqOverride, extraHeaders string, body []byte) []byte {
	return buildResponse(m.msg, status, toTag, cseqOverride, extraHeaders, body)
}

// Response487 terminates the INVITE this message (a CANCEL) cancelled.
func (m *SIPMessage) Response487(toTag string) []byte { return build487(m.msg, toTag) }

// URIOfHeader extracts the sip: URI from a From, To or Contact value.
func URIOfHeader(header string) string { return extractURI(header) }

// SDPOffer is the part of an SDP body the UAS acts on.
type SDPOffer = sdpOffer

// ParseSDP reads an offer.
func ParseSDP(sdp string) *SDPOffer { return parseSDP(sdp) }

// NegotiateCodec picks a law from the offer in the order of preferences.
func NegotiateCodec(offer *SDPOffer, preferences []media.Law) (media.Law, int, bool) {
	return negotiate(offer, preferences)
}

// BuildSDPAnswer writes the SDP carried in a 200 OK.
func BuildSDPAnswer(localIP string, localPort int, sessionID uint32,
	law media.Law, payloadType, dtmfPayloadType int) string {
	return buildSDPAnswer(localIP, localPort, sessionID, law, payloadType, dtmfPayloadType)
}

// BuildBye renders a BYE for a dialog this side accepted, with no Reason.
func BuildBye(callID, fromHeader, toHeader, localTag, localIP string,
	localPort int, recordRoutes []string, remoteContact string) []byte {
	return buildBye(callID, fromHeader, toHeader, localTag, localIP, localPort,
		recordRoutes, remoteContact, byeReason{})
}
