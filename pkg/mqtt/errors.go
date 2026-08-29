// Package mqtt implements the MQTT 3.1.1 wire codec for EdgeFlow v0.24.0.
// It has zero third-party dependencies.
package mqtt

import (
	"errors"
	"fmt"
)

// ErrMalformed is the sentinel error for any malformed packet.
var ErrMalformed = errors.New("mqtt: malformed packet")

// ErrMalformedVarint reports a bad remaining-length varint (>4 bytes or out of range).
var ErrMalformedVarint = fmt.Errorf("%w: varint", ErrMalformed)

// ErrMalformedFixedHeader reports an invalid fixed-header byte (bad flags or type).
var ErrMalformedFixedHeader = fmt.Errorf("%w: fixed header", ErrMalformed)

// ErrMalformedString reports a truncated or overlong UTF-8 string field.
var ErrMalformedString = fmt.Errorf("%w: string", ErrMalformed)

// ErrMalformedConnect reports a malformed CONNECT variable header or payload.
var ErrMalformedConnect = fmt.Errorf("%w: connect", ErrMalformed)

// ErrMalformedTopic reports an invalid topic name or filter.
var ErrMalformedTopic = fmt.Errorf("%w: topic", ErrMalformed)

// ErrShortBody reports a body that ends before the declared remaining length.
var ErrShortBody = fmt.Errorf("%w: short body", ErrMalformed)
