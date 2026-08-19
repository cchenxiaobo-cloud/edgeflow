// Package opcua implements the OPC UA binary protocol stack core
// (OPC UA Part 6, "Mappings — UA Binary", OPC 10000-6) as the
// foundation for the EdgeFlow OPC-UA adapter.
//
// It is written against the zero-third-party-dependency rule of
// EdgeFlow: only the Go standard library is used.
//
// # Scope (implemented in this milestone, WBS 5.2-M1)
//
//   - Built-in type system (types.go): NodeId (two-byte / four-byte /
//     numeric / string / guid / byte-string encodings, including the
//     extended 32-bit namespace-index forms), QualifiedName,
//     LocalizedText, StatusCode (with severity), Guid, ByteString,
//     ExtensionObject, DataValue (status / timestamps / picoseconds),
//     Variant (type-mask based, scalar + array round-trip), DateTime
//     (100 ns ticks since 1601-01-01).
//   - UA Binary codec (binary.go): big-endian primitives with the
//     Int32 length-prefix rules (-1 = null), UTF-8 strings with
//     decoder-side truncation at MaxStringLength (per Part 6 §5.1.4),
//     Guid as 16 bytes, NodeId encoding-byte rules, ExtensionObject
//     body length prefix.
//   - Message framing (message.go): MessageHeader (message type +
//     chunk type + MessageSize + ChannelId) and the Hello /
//     Acknowledge / Error messages.
//   - TCP transport (transport.go) with SecurityPolicy None: Dial
//     performs connect → Hello → Acknowledge validation and returns a
//     ready Conn; ReadMessage / WriteMessage provide single-chunk
//     frame-level I/O with length-overflow rejection.
//
// # Explicitly NOT implemented (later milestones)
//
//   - Read / Write / Subscribe services and any other service requests
//   - SecureChannel open / CloseSecureChannel. The Conn returned by
//     Dial is a raw, unsecured transport; ChannelId stays 0 until the
//     SecureChannel milestone.
//   - Security policies Sign / SignAndEncrypt; only SecurityPolicy
//     None (plaintext) exists.
//   - UA node model / object tree / discovery endpoints
//   - Client API for the Mapper layer (planned for a later milestone)
//   - ExpandedNodeId with namespace-URI / server-index, XmlElement and
//     DiagnosticInfo full bitfield semantics (DiagnosticInfo is a
//     skeleton; see types.go)
//   - Message chunking: MaxChunkCount is 1 and intermediate chunks
//     ('C') are rejected with ErrChunkingUnsupported.
//
// # Security limitation
//
// SecurityPolicy None transmits every byte in plaintext over TCP and
// provides no authentication or integrity protection. It must only be
// used in trusted, isolated networks (closed OT segments, localhost
// simulation). Never expose a SecurityPolicy None endpoint to an
// untrusted network.
package opcua
