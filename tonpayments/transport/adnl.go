package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"github.com/rs/zerolog/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/adnl/rldp"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"math/big"
	"sync"
	"time"
)

const _ChunkSize = 1 << 17
const _RLDPMaxAnswerSize = 2*_ChunkSize + 1024

type PeerConnection struct {
	rldp    *rldp.RLDP
	adnl    adnl.Peer
	authKey ed25519.PublicKey

	mx sync.Mutex
}

type Service interface {
	GetChannelConfig() ChannelConfig
	ProcessAction(ctx context.Context, key ed25519.PublicKey, channelAddr *address.Address, signedState payments.SignedSemiChannel, action Action) (*payments.SignedSemiChannel, error)
	ProcessActionRequest(ctx context.Context, key ed25519.PublicKey, channelAddr *address.Address, action Action) error
	ProcessInboundChannelRequest(ctx context.Context, capacity *big.Int, walletAddr *address.Address, key ed25519.PublicKey) error
}

type Server struct {
	svc        Service
	channelKey ed25519.PrivateKey
	key        ed25519.PrivateKey
	dht        *dht.Client
	gate       *adnl.Gateway
	closeCtx   context.Context

	peersByKey map[string]*PeerConnection
	peers      map[string]*PeerConnection
	mx         sync.RWMutex

	closer func()
}

func NewServer(dht *dht.Client, gate *adnl.Gateway, key, channelKey ed25519.PrivateKey, serverMode bool) *Server {
	s := &Server{
		channelKey: channelKey,
		key:        key,
		dht:        dht,
		gate:       gate,
		peersByKey: map[string]*PeerConnection{},
		peers:      map[string]*PeerConnection{},
	}
	s.closeCtx, s.closer = context.WithCancel(context.Background())
	s.gate.SetConnectionHandler(s.bootstrapPeerWrap)

	if serverMode {
		go func() {
			wait := 1 * time.Second
			// refresh dht records
			for {
				select {
				case <-s.closeCtx.Done():
					log.Info().Str("source", "server").Msg("stopped dht updater")
					return
				case <-time.After(wait):
				}

				log.Debug().Str("source", "server").Msg("updating our dht record")

				ctx, cancel := context.WithTimeout(s.closeCtx, 100*time.Second)
				err := s.updateDHT(ctx)
				cancel()

				if err != nil {
					log.Warn().Err(err).Str("source", "server").Msg("failed to update our dht record, will retry in 5 sec")

					// on err, retry sooner
					wait = 5 * time.Second
					continue
				}
				wait = 1 * time.Minute
			}
		}()
	}
	return s
}

func (s *Server) SetService(svc Service) {
	s.svc = svc
}

func (s *Server) updateDHT(ctx context.Context) error {
	addr := s.gate.GetAddressList()

	ctxStore, cancel := context.WithTimeout(ctx, 80*time.Second)
	stored, id, err := s.dht.StoreAddress(ctxStore, addr, 10*time.Minute, s.key, 5)
	cancel()
	if err != nil && stored == 0 {
		return err
	}

	// make sure it was saved
	_, _, err = s.dht.FindAddresses(ctx, id)
	if err != nil {
		return err
	}
	log.Debug().Str("source", "server").Int("copies", stored).Msg("our address was updated in dht")

	chanKey := adnl.PublicKeyED25519{Key: s.channelKey.Public().(ed25519.PublicKey)}
	dhtVal, err := tl.Serialize(NodeAddress{
		ADNLAddr: id,
	}, true)
	if err != nil {
		return err
	}

	stored, _, err = s.dht.Store(ctx, chanKey, []byte("payment-node"), 0,
		dhtVal, dht.UpdateRuleSignature{}, 10*time.Minute, s.channelKey, 5)
	if err != nil {
		return fmt.Errorf("failed to store node payment-node value in dht: %w", err)
	}
	log.Debug().Str("source", "server").Int("copies", stored).Msg("our payment-node adnl address was updated in dht")

	return nil
}

func (s *Server) bootstrapPeerWrap(client adnl.Peer) error {
	s.bootstrapPeer(client)
	return nil
}

func (s *Server) bootstrapPeer(client adnl.Peer) *PeerConnection {
	s.mx.Lock()
	defer s.mx.Unlock()

	if rl := s.peers[string(client.GetID())]; rl != nil {
		return rl
	}

	rl := rldp.NewClientV2(client)
	p := &PeerConnection{
		rldp: rl,
		adnl: client,
	}

	rl.SetOnQuery(s.handleRLDPQuery(p))

	rl.SetOnDisconnect(func() {
		s.mx.Lock()
		if p.authKey != nil {
			log.Info().Hex("key", p.authKey).Msg("peer disconnected")

			delete(s.peersByKey, string(p.authKey))
		}
		delete(s.peers, string(p.adnl.GetID()))
		s.mx.Unlock()
	})

	s.peers[string(client.GetID())] = p

	return p
}

func (s *Server) handleRLDPQuery(peer *PeerConnection) func(transfer []byte, query *rldp.Query) error {
	return func(transfer []byte, query *rldp.Query) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		switch q := query.Data.(type) {
		case Authenticate:
			if q.Timestamp < time.Now().Add(-30*time.Second).Unix() || q.Timestamp > time.Now().Unix() {
				return fmt.Errorf("outdated auth data")
			}

			// check signature with both adnl addresses, to protect from MITM attack
			authData, err := tl.Hash(AuthenticateToSign{
				A:         peer.adnl.GetID(),
				B:         s.gate.GetID(),
				Timestamp: q.Timestamp,
			})
			if err != nil {
				return fmt.Errorf("failed to hash their auth data: %w", err)
			}

			if !ed25519.Verify(q.Key, authData, q.Signature) {
				return fmt.Errorf("incorrect signature")
			}

			s.mx.Lock()
			if peer.authKey != nil {
				// when authenticated with new key, delete old record
				delete(s.peersByKey, string(peer.authKey))
			}
			peer.authKey = append([]byte{}, q.Key...)
			s.peersByKey[string(peer.authKey)] = peer
			s.mx.Unlock()
			log.Info().Hex("key", peer.authKey).Msg("connected with peer")

			// reverse A and B, and sign, so party can verify us too
			authData, err = tl.Hash(AuthenticateToSign{
				A:         s.gate.GetID(),
				B:         peer.adnl.GetID(),
				Timestamp: q.Timestamp,
			})
			if err != nil {
				return fmt.Errorf("failed to hash our auth data: %w", err)
			}

			if err = peer.rldp.SendAnswer(ctx, query.MaxAnswerSize, query.ID, transfer, Authenticate{
				Key:       s.channelKey.Public().(ed25519.PublicKey),
				Timestamp: q.Timestamp,
				Signature: ed25519.Sign(s.channelKey, authData),
			}); err != nil {
				return err
			}
		case GetChannelConfig:
			if err := peer.rldp.SendAnswer(ctx, query.MaxAnswerSize, query.ID, transfer, s.svc.GetChannelConfig()); err != nil {
				return err
			}
		case RequestInboundChannel:
			res := Decision{Agreed: true}
			err := s.svc.ProcessInboundChannelRequest(ctx, new(big.Int).SetBytes(q.Capacity), address.NewAddress(0, 0, q.Wallet), q.Key)
			if err != nil {
				res.Agreed = false
				res.Reason = err.Error()
			}

			if err = peer.rldp.SendAnswer(ctx, query.MaxAnswerSize, query.ID, transfer, res); err != nil {
				return err
			}
		case ProposeAction:
			if peer.authKey == nil {
				return fmt.Errorf("not authorized")
			}

			var state payments.SignedSemiChannel
			if err := tlb.LoadFromCell(&state, q.SignedState.BeginParse()); err != nil {
				return fmt.Errorf("failed to parse channel state")
			}

			var updCell *cell.Cell
			ok := true
			reason := ""
			updateProof, err := s.svc.ProcessAction(ctx, peer.authKey,
				address.NewAddress(0, 0, q.ChannelAddr), state, q.Action)
			if err != nil {
				reason = err.Error()
				ok = false
			} else {
				if updCell, err = tlb.ToCell(updateProof); err != nil {
					return fmt.Errorf("failed to serialize state cell: %w", err)
				}
			}

			if err := peer.rldp.SendAnswer(ctx, query.MaxAnswerSize, query.ID, transfer, ProposalDecision{Agreed: ok, Reason: reason, SignedState: updCell}); err != nil {
				return err
			}
		case RequestAction:
			if peer.authKey == nil {
				return fmt.Errorf("not authorized")
			}

			ok := true
			reason := ""
			if err := s.svc.ProcessActionRequest(ctx, peer.authKey,
				address.NewAddress(0, 0, q.ChannelAddr), q.Action); err != nil {
				reason = err.Error()
				ok = false
			}

			if err := peer.rldp.SendAnswer(ctx, query.MaxAnswerSize, query.ID, transfer, Decision{Agreed: ok, Reason: reason}); err != nil {
				return err
			}
		}
		return nil
	}
}

func (s *Server) connect(ctx context.Context, channelKey ed25519.PublicKey) (*PeerConnection, error) {
	channelKeyId, err := tl.Hash(adnl.PublicKeyED25519{Key: channelKey})
	if err != nil {
		return nil, fmt.Errorf("failed to calc hash of channel key %s: %w", hex.EncodeToString(channelKey), err)
	}

	dhtVal, _, err := s.dht.FindValue(ctx, &dht.Key{
		ID:    channelKeyId,
		Name:  []byte("payment-node"),
		Index: 0,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find address in dht of %s: %w", hex.EncodeToString(channelKey), err)
	}

	var nodeAddr NodeAddress
	if _, err = tl.Parse(&nodeAddr, dhtVal.Data, true); err != nil {
		return nil, fmt.Errorf("failed to parse node dht value of %s: %w", hex.EncodeToString(channelKey), err)
	}

	list, key, err := s.dht.FindAddresses(ctx, nodeAddr.ADNLAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to find address in dht of %s: %w", hex.EncodeToString(channelKey), err)
	}

	if len(list.Addresses) == 0 {
		return nil, fmt.Errorf("no addresses for %s", hex.EncodeToString(channelKey))
	}
	addr := fmt.Sprintf("%s:%d", list.Addresses[0].IP.String(), list.Addresses[0].Port)

	peer, err := s.gate.RegisterClient(addr, key)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to peer of %s at %s: %w", hex.EncodeToString(channelKey), addr, err)
	}
	return s.bootstrapPeer(peer), nil
}

func (s *Server) auth(ctx context.Context, peer *PeerConnection) error {
	ts := time.Now().Unix()
	authData, err := tl.Hash(AuthenticateToSign{
		A:         s.gate.GetID(),
		B:         peer.adnl.GetID(),
		Timestamp: ts,
	})
	if err != nil {
		return fmt.Errorf("failed to hash our auth data: %w", err)
	}

	var res Authenticate
	err = peer.rldp.DoQuery(ctx, _RLDPMaxAnswerSize, Authenticate{
		Key:       s.channelKey.Public().(ed25519.PublicKey),
		Timestamp: ts,
		Signature: ed25519.Sign(s.channelKey, authData),
	}, &res)
	if err != nil {
		return fmt.Errorf("failed to request auth: %w", err)
	}

	authData, err = tl.Hash(AuthenticateToSign{
		A:         peer.adnl.GetID(),
		B:         s.gate.GetID(),
		Timestamp: ts,
	})
	if err != nil {
		return fmt.Errorf("failed to hash their auth data: %w", err)
	}

	if !ed25519.Verify(res.Key, authData, res.Signature) {
		return fmt.Errorf("incorrect response signature")
	}

	s.mx.Lock()
	if peer.authKey != nil {
		// when authenticated with new key, delete old record
		delete(s.peersByKey, string(peer.authKey))
	}
	peer.authKey = append([]byte{}, res.Key...)
	s.peersByKey[string(peer.authKey)] = peer
	s.mx.Unlock()
	log.Info().Hex("key", peer.authKey).Msg("connected with peer")

	return nil
}

func (s *Server) preparePeer(ctx context.Context, key []byte) (peer *PeerConnection, err error) {
	if bytes.Equal(key, s.channelKey.Public().(ed25519.PublicKey)) {
		return nil, fmt.Errorf("cannot connect to ourself")
	}

	s.mx.RLock()
	peer = s.peersByKey[string(key)]
	s.mx.RUnlock()

	if peer == nil {
		if peer, err = s.connect(ctx, key); err != nil {
			return nil, fmt.Errorf("failed to connect to peer: %w", err)
		}
	}

	peer.mx.Lock()
	defer peer.mx.Unlock()

	if peer.authKey == nil {
		if err = s.auth(ctx, peer); err != nil {
			return nil, fmt.Errorf("failed to auth peer: %w", err)
		}
	}

	return peer, nil
}

func (s *Server) GetChannelConfig(ctx context.Context, theirChannelKey ed25519.PublicKey) (*ChannelConfig, error) {
	var res ChannelConfig
	err := s.doQuery(ctx, theirChannelKey, GetChannelConfig{}, &res)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	return &res, nil
}

func (s *Server) ProposeAction(ctx context.Context, channelAddr *address.Address, theirChannelKey []byte, state *cell.Cell, action Action) (*ProposalDecision, error) {
	var res ProposalDecision
	err := s.doQuery(ctx, theirChannelKey, ProposeAction{
		ChannelAddr: channelAddr.Data(),
		Action:      action,
		SignedState: state,
	}, &res)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	return &res, nil
}

func (s *Server) RequestAction(ctx context.Context, channelAddr *address.Address, theirChannelKey []byte, action Action) (*Decision, error) {
	var res Decision
	err := s.doQuery(ctx, theirChannelKey, RequestAction{
		ChannelAddr: channelAddr.Data(),
		Action:      action,
	}, &res)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	return &res, nil
}

func (s *Server) RequestInboundChannel(ctx context.Context, capacity *big.Int, ourWallet *address.Address, ourKey, theirKey []byte) (*Decision, error) {
	var res Decision
	err := s.doQuery(ctx, theirKey, RequestInboundChannel{
		Key:      ourKey,
		Wallet:   ourWallet.Data(),
		Capacity: capacity.Bytes(),
	}, &res)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	return &res, nil
}

func (s *Server) doQuery(ctx context.Context, theirKey []byte, req, resp tl.Serializable) error {
	peer, err := s.preparePeer(ctx, theirKey)
	if err != nil {
		return fmt.Errorf("failed to prepare peer: %w", err)
	}

	var cancel func()
	dl, ok := ctx.Deadline()
	if !ok || dl.After(time.Now().Add(7*time.Second)) {
		ctx, cancel = context.WithTimeout(ctx, 7*time.Second)
		defer cancel()
	}

	tm := time.Now()
	err = peer.rldp.DoQuery(ctx, _RLDPMaxAnswerSize, req, resp)
	if err != nil {
		// TODO: check other network cases too
		if time.Since(tm) > 3*time.Second {
			// drop peer to reconnect
			peer.adnl.Close()
		}
		return fmt.Errorf("failed to make request: %w", err)
	}
	return nil
}
