package noise

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/flynn/noise"
	"github.com/gogo/protobuf/proto"

	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"

	"github.com/libp2p/go-libp2p-noise/pb"
)

// payloadSigPrefix is prepended to our Noise static key before signing with
// our libp2p identity key.
const payloadSigPrefix = "noise-libp2p-static-key:"

// All noise session share a fixed cipher suite
var cipherSuite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)

// runHandshake exchanges handshake messages with the remote peer to establish
// a noise-libp2p session. It blocks until the handshake completes or fails.
func (s *secureSession) runHandshake(ctx context.Context) error {
	kp, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return fmt.Errorf("error generating static keypair: %w", err)
	}

	cfg := noise.Config{
		CipherSuite:   cipherSuite,
		Pattern:       noise.HandshakeXX,
		Initiator:     s.initiator,
		StaticKeypair: kp,
	}

	hs, err := noise.NewHandshakeState(cfg)
	if err != nil {
		return fmt.Errorf("error initializing handshake state: %w", err)
	}

	payload, err := s.generateHandshakePayload(kp)
	if err != nil {
		return err
	}

	if s.initiator {
		// stage 0 //
		// do not send the payload just yet, as it would be plaintext; not secret.
		err = s.sendHandshakeMessage(hs, nil)
		if err != nil {
			return fmt.Errorf("error sending handshake message: %w", err)
		}

		// stage 1 //
		plaintext, err := s.readHandshakeMessage(hs)
		if err != nil {
			return fmt.Errorf("error reading handshake message: %w", err)
		}
		err = s.handleRemoteHandshakePayload(plaintext, hs.PeerStatic())
		if err != nil {
			return err
		}

		// stage 2 //
		err = s.sendHandshakeMessage(hs, payload)
		if err != nil {
			return fmt.Errorf("error sending handshake message: %w", err)
		}
	} else {
		// stage 0 //
		plaintext, err := s.readHandshakeMessage(hs)
		if err != nil {
			return fmt.Errorf("error reading handshake message: %w", err)
		}

		// stage 1 //
		err = s.sendHandshakeMessage(hs, payload)
		if err != nil {
			return fmt.Errorf("error sending handshake message: %w", err)
		}

		// stage 2 //
		plaintext, err = s.readHandshakeMessage(hs)
		if err != nil {
			return fmt.Errorf("error reading handshake message: %w", err)
		}
		err = s.handleRemoteHandshakePayload(plaintext, hs.PeerStatic())
		if err != nil {
			return err
		}
	}

	return nil
}

// setCipherStates sets the initial cipher states that will be used to protect traffic after the handshake.
//
// It is called when the final handshake message is processed by
// either sendHandshakeMessage or readHandshakeMessage.
func (s *secureSession) setCipherStates(cs1, cs2 *noise.CipherState) {
	if s.initiator {
		s.enc = cs1
		s.dec = cs2
	} else {
		s.enc = cs2
		s.dec = cs1
	}
}

// sendHandshakeMessage sends the next handshake message in the sequence.
//
// If payload is non-empty, it will be included in the handshake message.
// If this is the final message in the sequence, calls setCipherStates
// to initialize cipher states.
func (s *secureSession) sendHandshakeMessage(hs *noise.HandshakeState, payload []byte) error {
	buf, cs1, cs2, err := hs.WriteMessage(nil, payload)
	if err != nil {
		return err
	}

	err = s.writeMsgInsecure(buf)
	if err != nil {
		return err
	}

	if cs1 != nil && cs2 != nil {
		s.setCipherStates(cs1, cs2)
	}
	return nil
}

// readHandshakeMessage reads a message from the insecure conn and tries to
// process it as the expected next message in the handshake sequence.
//
// If the message contains a payload, it will be decrypted and returned.
//
// If this is the final message in the sequence, it calls setCipherStates
// to initialize cipher states.
func (s *secureSession) readHandshakeMessage(hs *noise.HandshakeState) ([]byte, error) {
	raw, err := s.readMsgInsecure()
	if err != nil {
		return nil, err
	}
	msg, cs1, cs2, err := hs.ReadMessage(nil, raw)
	if err != nil {
		return nil, err
	}
	if cs1 != nil && cs2 != nil {
		s.setCipherStates(cs1, cs2)
	}
	return msg, nil
}

// generateHandshakePayload creates a libp2p handshake payload with a
// signature of our static noise key.
func (s *secureSession) generateHandshakePayload(localStatic noise.DHKey) ([]byte, error) {
	// obtain the public key from the handshake session so we can sign it with
	// our libp2p secret key.
	localKeyRaw, err := s.LocalPublicKey().Bytes()
	if err != nil {
		return nil, fmt.Errorf("error serializing libp2p identity key: %w", err)
	}

	// prepare payload to sign; perform signature.
	toSign := append([]byte(payloadSigPrefix), localStatic.Public...)
	signedPayload, err := s.localKey.Sign(toSign)
	if err != nil {
		return nil, fmt.Errorf("error sigining handshake payload: %w", err)
	}

	// create payload
	payload := new(pb.NoiseHandshakePayload)
	payload.IdentityKey = localKeyRaw
	payload.IdentitySig = signedPayload
	payloadEnc, err := proto.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("error marshaling handshake payload: %w", err)
	}
	return payloadEnc, nil
}

// handleRemoteHandshakePayload unmarshals the handshake payload object sent
// by the remote peer and validates the signature against the peer's static Noise key.
func (s *secureSession) handleRemoteHandshakePayload(payload []byte, remoteStatic []byte) error {
	// unmarshal payload
	nhp := new(pb.NoiseHandshakePayload)
	err := proto.Unmarshal(payload, nhp)
	if err != nil {
		return fmt.Errorf("error unmarshaling remote handshake payload: %w", err)
	}

	// unpack remote peer's public libp2p key
	remotePubKey, err := crypto.UnmarshalPublicKey(nhp.GetIdentityKey())
	if err != nil {
		return err
	}
	id, err := peer.IDFromPublicKey(remotePubKey)
	if err != nil {
		return err
	}

	// if we know who we're trying to reach, make sure we have the right peer
	if s.initiator && s.remoteID != id {
		// use Pretty() as it produces the full b58-encoded string, rather than abbreviated forms.
		return fmt.Errorf("peer id mismatch: expected %s, but remote key matches %s", s.remoteID.Pretty(), id.Pretty())
	}

	// verify payload is signed by asserted remote libp2p key.
	sig := nhp.GetIdentitySig()
	msg := append([]byte(payloadSigPrefix), remoteStatic...)
	ok, err := remotePubKey.Verify(msg, sig)
	if err != nil {
		return fmt.Errorf("error verifying signature: %w", err)
	} else if !ok {
		return fmt.Errorf("handshake signature invalid")
	}

	// set remote peer key and id
	s.remoteID = id
	s.remoteKey = remotePubKey
	return nil
}
