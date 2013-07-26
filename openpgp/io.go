/*
   Hockeypuck - OpenPGP key server
   Copyright (C) 2012, 2013  Casey Marshall

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package openpgp

import (
	packetErrors "code.google.com/p/go.crypto/openpgp/errors"
	"code.google.com/p/go.crypto/openpgp/packet"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"sort"
)

// Comparable time flag for "never expires"
const NeverExpires = int64((1 << 63) - 1)

// Get the public key fingerprint as a hex string.
func Fingerprint(pubkey *packet.PublicKey) string {
	return hex.EncodeToString(pubkey.Fingerprint[:])
}

func WriteTo(w io.Writer, root PacketRecord) error {
	return root.Visit(func(rec PacketRecord) error {
		op, err := rec.GetOpaquePacket()
		if err != nil {
			return err
		}
		return op.Serialize(w)
	})
}

type OpaquePacketResult struct {
	*packet.OpaquePacket
	err error
}

type OpaquePacketChan chan *OpaquePacketResult

func IterOpaquePackets(root PacketRecord) OpaquePacketChan {
	c := make(OpaquePacketChan)
	go func() {
		defer close(c)
		root.Visit(func(rec PacketRecord) error {
			op, err := rec.GetOpaquePacket()
			c <- &OpaquePacketResult{op, err}
			if err != nil {
				return err
			}
		})
	}()
	return c
}

// SksDigest calculates a cumulative message digest on all
// OpenPGP packets for a given primary public key,
// using the same ordering as SKS, the Synchronizing Key Server.
// Use MD5 for matching digest values with SKS.
func SksDigest(key *Pubkey, h hash.Hash) string {
	var packets packetSlice
	for opkt := range IterOpaquePackets(key) {
		packets = append(packets, opkt.OpaquePacket)
	}
	sort.Sort(sksPacketSorter{packets})
	for _, opkt := range packets {
		binary.Write(h, binary.BigEndian, int32(opkt.Tag))
		binary.Write(h, binary.BigEndian, int32(len(opkt.Contents)))
		h.Write(opkt.Contents)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type ReadKeyResult struct {
	*Pubkey
	Error error
}

type PubkeyChan chan *ReadKeyResult

func ErrReadKeys(msg string) *ReadKeyResult {
	return &ReadKeyResult{Error: errors.New(msg)}
}

// Read one or more public keys from input.
func ReadKeys(r io.Reader) PubkeyChan {
	c := make(PubkeyChan)
	go func() {
		defer close(c)
		var err error
		var parseErr error
		var opkt *packet.OpaquePacket
		var currentPubkey *Pubkey
		var currentSignable Signable
		var fingerprint string
		opktReader := packet.NewOpaqueReader(r)
		for opkt, err = opktReader.Next(); err != io.EOF; opkt, err = opktReader.Next() {
			if err != nil {
				c <- &ReadKeyResult{Error: err}
				return
			}
			pkt, parseErr := opkt.Parse()
			if parseErr == nil {
				switch p := pkt.(type) {
				case *packet.PublicKey:
					if !p.IsSubkey {
						if currentPubkey != nil {
							// New public key found, send prior one
							c <- &ReadKeyResult{Pubkey: currentPubkey}
							currentPubkey = nil
						}
						pubkey := new(Pubkey)
						if err = pubkey.SetPublicKey(p); err != nil {
							currentPubkey = nil
							c <- &ReadKeyResult{Error: err}
							continue
						}
						currentPubkey = pubkey
						currentSignable = currentPubkey
					} else {
						if currentPubkey == nil {
							c <- ErrReadKeys(
								"Subkey outside of primary public key scope in stream")
							continue
						}
						// This is a sub key
						subkey := new(Subkey)
						if err = subkey.SetPublicKey(p); err != nil {
							c <- &ReadKeyResult{Error: err}
						}
						currentPubkey.subkeys = append(currentPubkey.subkeys, *subkey)
						currentSignable = subkey
					}
				case *packet.Signature:
					if currentSignable == nil {
						c <- ErrReadKeys("Signature outside signable scope in stream")
						continue
					}
					sig := new(Signature)
					if err = sig.SetSignature(p); err != nil {
						c <- &ReadKeyResult{Error: err}
						continue
					}
					currentSignable.AddSignature(sig)
				case *packet.UserId:
					if currentPubkey == nil {
						c <- ErrReadKeys("User ID outside primary public key scope in stream")
						continue
					}
					uid := new(UserId)
					if err = uid.SetUserId(p); err != nil {
						c <- &ReadKeyResult{Error: err}
						continue
					}
					currentSignable = uid
					currentPubkey.userIds = append(currentPubkey.userIds, *uid)
				}
			}
			if _, isUnknown := parseErr.(packetErrors.UnknownPacketTypeError); isUnknown {
				// Packets not yet supported by go.crypto/openpgp
				switch opkt.Tag {
				case 17: // Process user attribute packet
					if currentPubkey == nil {
						c <- ErrReadKeys(
							"User attribute outside primary public key scope in stream")
						continue
					}
					uat := new(UserAttribute)
					if err = uat.SetPacket(opkt); err != nil {
						c <- &ReadKeyResult{Error: err}
						continue
					}
					currentSignable = uat
					currentPubkey.userAttributes = append(currentPubkey.userAttributes, *uat)
				case 2: // Bad signature packet
					// TODO: Check for signature version 3
					c <- &ReadKeyResult{Error: parseErr}
				case 6: // Bad public key packet
					// TODO: Check for unsupported PGP public key packet version
					// For now, clear state, ignore to next key
					if currentPubkey != nil {
						// Send prior public key, if any
						c <- &ReadKeyResult{Pubkey: currentPubkey}
						currentPubkey = nil
					}
					c <- &ReadKeyResult{Error: parseErr}
					currentPubkey = nil
					currentSignable = nil
				default:
					c <- &ReadKeyResult{Error: parseErr}
				}
			}
		}
		if currentPubkey != nil {
			c <- &ReadKeyResult{Pubkey:currentPubkey}
		}
	}()
	return c
}