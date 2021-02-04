/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package u2f

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/mailgun/ttlmap"
	"github.com/tstranex/u2f"

	"github.com/gravitational/teleport/api/types"
)

// Authentication sequence:
//
//    *client*                      *messages over network*            *server*
//
//                                                                 AuthenticateInit()
//                             <------ AuthenticateChallenge -----
// AuthenticateSignChallenge()
//                             -- AuthenticateChallengeResponse ->
//                                                                 AuthenticateVerify()

type (
	// AuthenticateChallenge is the first message in authentication sequence.
	// It's sent from the server to the client.
	AuthenticateChallenge = u2f.SignRequest
	// AuthenticateChallengeResponse is the second message in authentication
	// sequence. It's sent from the client to the server in response to
	// AuthenticateChallenge.
	AuthenticateChallengeResponse = u2f.SignResponse
)

// AuthenticationStorage is the persistent storage needed to store state
// (challenges and counters) during the authentication sequence.
type AuthenticationStorage interface {
	DeviceStorage

	UpsertU2FSignChallenge(user, deviceID string, c *Challenge) error
	GetU2FSignChallenge(user, deviceID string) (*Challenge, error)
}

const (
	// Set capacity at 6000. With 60s TTLs on challenges, this allows roughly
	// 100 U2F authentications/registration per second. Any larger burst or
	// sustained rate will evict oldest challenges.
	inMemoryChallengeCapacity = 6000
	inMemoryChallengeTTL      = 60 * time.Second
)

type inMemoryAuthenticationStorage struct {
	DeviceStorage
	challenges *ttlmap.TtlMap
}

// InMemoryAuthenticationStorage returns a new AuthenticationStorage that
// stores authentication challenges in the current process memory.
//
// Updates to existing devices are forwarded to ds.
func InMemoryAuthenticationStorage(ds DeviceStorage) (AuthenticationStorage, error) {
	m, err := ttlmap.NewMap(inMemoryChallengeCapacity)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return inMemoryAuthenticationStorage{DeviceStorage: ds, challenges: m}, nil
}

func (s inMemoryAuthenticationStorage) key(user, deviceID string) string {
	return fmt.Sprintf("%s-%s", user, deviceID)
}

func (s inMemoryAuthenticationStorage) UpsertU2FSignChallenge(user, deviceID string, c *Challenge) error {
	return s.challenges.Set(s.key(user, deviceID), c, int(inMemoryChallengeTTL.Seconds()))
}

func (s inMemoryAuthenticationStorage) GetU2FSignChallenge(user, deviceID string) (*Challenge, error) {
	v, ok := s.challenges.Get(s.key(user, deviceID))
	if !ok {
		return nil, trace.NotFound("U2F challenge not found or expired")
	}
	c, ok := v.(*Challenge)
	if !ok {
		return nil, trace.NotFound("bug: U2F challenge storage returned %T instead of *u2f.Challenge", v)
	}
	return c, nil
}

// AuthenticateInitParams are the parameters for initiating the authentication
// sequence.
type AuthenticateInitParams struct {
	AppConfig  types.U2F
	Dev        *types.MFADevice
	StorageKey string
	Storage    AuthenticationStorage
}

// AuthenticateInit is the first step in the authentication sequence. It runs
// on the server and the returned AuthenticateChallenge must be sent to the
// client.
func AuthenticateInit(ctx context.Context, params AuthenticateInitParams) (*AuthenticateChallenge, error) {
	if params.Dev == nil {
		return nil, trace.BadParameter("bug: missing Dev field in u2f.AuthenticateInitParams")
	}
	dev := params.Dev.GetU2F()
	if dev == nil {
		return nil, trace.BadParameter("bug: u2f.AuthenticateInit called with %T instead of MFADevice_U2F", params.Dev.Device)
	}
	reg, err := DeviceToRegistration(dev)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	challenge, err := NewChallenge(params.AppConfig.AppID, params.AppConfig.Facets)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err = params.Storage.UpsertU2FSignChallenge(params.StorageKey, params.Dev.Id, challenge); err != nil {
		return nil, trace.Wrap(err)
	}

	return challenge.SignRequest(*reg), nil
}

// AuthenticateSignChallenge is the second step in the authentication sequence.
// It runs on the client and the returned AuthenticationChallengeResponse must
// be sent to the server.
//
// Note: the caller must prompt the user to tap the U2F token.
func AuthenticateSignChallenge(ctx context.Context, facet string, challenges ...AuthenticateChallenge) (*AuthenticateChallengeResponse, error) {
	// TODO(awly): mfa: u2f-host fails when running multiple processes in
	// parallel.  This means that with u2f-host, teleport can't authenticate
	// using multiple U2F devices. Replace u2f-host with a Go library that can
	// prompt multiple devices at once.
	c := challenges[0]

	// Pass the JSON-encoded data undecoded to the u2f-host binary
	challengeRaw, err := json.Marshal(c)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	cmd := exec.CommandContext(ctx, "u2f-host", "-aauthenticate", "-o", facet)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if err := cmd.Start(); err != nil {
		return nil, trace.Wrap(err)
	}
	defer func() {
		// If we returned before cmd.Wait was called, clean up the spawned
		// process. ProcessState will be empty until cmd.Wait or cmd.Run
		// return.
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			cmd.Process.Kill()
		}
	}()
	_, err = stdin.Write(challengeRaw)
	stdin.Close()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// The origin URL is passed back base64-encoded and the keyHandle is passed back as is.
	// A very long proxy hostname or keyHandle can overflow a fixed-size buffer.
	signResponseLen := 500 + len(challengeRaw) + len(facet)*4/3
	signResponseBuf := make([]byte, signResponseLen)
	signResponseLen, err = io.ReadFull(stdout, signResponseBuf)
	// unexpected EOF means we have read the data completely.
	if err == nil {
		return nil, trace.LimitExceeded("u2f sign response exceeded buffer size")
	}

	// Read error message (if any). 100 bytes is more than enough for any error message u2f-host outputs
	errMsgBuf := make([]byte, 100)
	errMsgLen, err := io.ReadFull(stderr, errMsgBuf)
	if err == nil {
		return nil, trace.LimitExceeded("u2f error message exceeded buffer size")
	}

	err = cmd.Wait()
	if err != nil {
		return nil, trace.AccessDenied("u2f-host returned error: " + string(errMsgBuf[:errMsgLen]))
	} else if signResponseLen == 0 {
		return nil, trace.NotFound("u2f-host returned no error and no sign response")
	}

	var resp AuthenticateChallengeResponse
	if err := json.Unmarshal(signResponseBuf[:signResponseLen], &resp); err != nil {
		return nil, trace.Wrap(err)
	}
	return &resp, nil
}

// AuthenticateVerifyParams are the parameters for verifying the
// AuthenticationChallengeResponse.
type AuthenticateVerifyParams struct {
	Dev        *types.MFADevice
	Resp       AuthenticateChallengeResponse
	StorageKey string
	Storage    AuthenticationStorage
	Clock      clockwork.Clock
}

// AuthenticateVerify is the last step in the authentication sequence. It runs
// on the server and verifies the AuthenticateChallengeResponse returned by the
// client.
func AuthenticateVerify(ctx context.Context, params AuthenticateVerifyParams) error {
	if params.Dev == nil {
		return trace.BadParameter("no MFADevice provided")
	}
	dev := params.Dev.GetU2F()
	if dev == nil {
		return trace.BadParameter("provided MFADevice is not a U2FDevice: %T", params.Dev.Device)
	}
	reg, err := DeviceToRegistration(dev)
	if err != nil {
		return trace.Wrap(err)
	}
	challenge, err := params.Storage.GetU2FSignChallenge(params.StorageKey, params.Dev.Id)
	if err != nil {
		return trace.Wrap(err)
	}
	dev.Counter, err = reg.Authenticate(params.Resp, *challenge, dev.Counter)
	if err != nil {
		return trace.Wrap(err)
	}
	params.Dev.LastUsed = params.Clock.Now()
	if err := params.Storage.UpsertMFADevice(ctx, params.StorageKey, params.Dev); err != nil {
		return trace.Wrap(err)
	}
	return nil
}