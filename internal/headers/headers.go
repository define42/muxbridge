package headers

import (
	"net/http"

	"github.com/define42/muxbridge/gen/tunnelpb"
)

func ToProto(h http.Header) []*tunnelpb.Header {
	out := make([]*tunnelpb.Header, 0, len(h))
	for k, v := range h {
		vals := append([]string(nil), v...)
		out = append(out, &tunnelpb.Header{Key: k, Values: vals})
	}
	return out
}

func FromProto(in []*tunnelpb.Header) http.Header {
	h := make(http.Header)
	for _, kv := range in {
		if kv == nil {
			continue
		}
		vals := append([]string(nil), kv.Values...)
		h[kv.Key] = vals
	}
	return h
}

func Copy(dst, src http.Header) {
	for k, v := range src {
		for _, vv := range v {
			dst.Add(k, vv)
		}
	}
}
