package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/combust-labs/firebuild-shared/build/commands"
	"github.com/combust-labs/firebuild-shared/grpc/proto"
	"github.com/hashicorp/go-hclog"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// TestServer wraps an instance of a server and provides testing
// utilities around it.
type TestServer interface {
	Start()
	Stop()
	FailedNotify() <-chan error
	FinishedNotify() <-chan struct{}
	ReadyNotify() <-chan struct{}

	Aborted() error
	ConsumedStderr() []string
	ConsumedStdout() []string
	Succeeded() bool
}

// NewTest starts a new test server provider.
func NewTestServer(t *testing.T, logger hclog.Logger, cfg *GRPCServiceConfig, ctx *WorkContext) *testGRPCServerProvider {
	return &testGRPCServerProvider{
		cfg:          cfg,
		ctx:          ctx,
		logger:       logger,
		stdErrOutput: []string{},
		stdOutOutput: []string{},
		chanAborted:  make(chan struct{}),
		chanFailed:   make(chan error, 1),
		chanFinished: make(chan struct{}),
		chanReady:    make(chan struct{}),
	}
}

type testGRPCServerProvider struct {
	cfg *GRPCServiceConfig
	ctx *WorkContext
	srv Provider

	logger hclog.Logger

	abortError   error
	stdErrOutput []string
	stdOutOutput []string
	success      bool

	chanAborted  chan struct{}
	chanFailed   chan error
	chanFinished chan struct{}
	chanReady    chan struct{}

	isAbortedClosed bool
}

// Start starts a testing server.
func (p *testGRPCServerProvider) Start() {
	p.srv = New(p.cfg, p.logger)
	p.srv.Start(p.ctx)

	select {
	case <-p.srv.ReadyNotify():
		close(p.chanReady)
	case err := <-p.srv.FailedNotify():
		p.chanFailed <- err
		return
	}

	go func() {
	out:
		for {
			select {
			case <-p.srv.StoppedNotify():
				close(p.chanFinished)
				break out
			case stdErrLine := <-p.srv.OnStderr():
				if stdErrLine == "" {
					continue
				}
				p.stdErrOutput = append(p.stdErrOutput, stdErrLine)
			case stdOutLine := <-p.srv.OnStdout():
				if stdOutLine == "" {
					continue
				}
				p.stdOutOutput = append(p.stdOutOutput, stdOutLine)
			case outErr := <-p.srv.OnAbort():
				p.abortError = outErr
				close(p.chanAborted)
			case <-p.srv.OnSuccess():
				if p.success {
					continue
				}
				p.success = true
				go func() {
					p.srv.Stop()
				}()
			case <-p.chanAborted:
				if p.isAbortedClosed {
					continue
				}
				p.isAbortedClosed = true
				go func() {
					p.srv.Stop()
				}()
			}
		}
	}()
}

// Stop stops a testing server.
func (p *testGRPCServerProvider) Stop() {
	if p.srv != nil {
		p.srv.Stop()
	}
}

// FailedNotify returns a channel which will contain an error if the testing server failed to start.
func (p *testGRPCServerProvider) FailedNotify() <-chan error {
	return p.chanFailed
}

// FinishedNotify returns a channel which will be closed when the server is stopped.
func (p *testGRPCServerProvider) FinishedNotify() <-chan struct{} {
	return p.chanFinished
}

// ReadyNotify returns a channel which will be closed when the server is ready.
func (p *testGRPCServerProvider) ReadyNotify() <-chan struct{} {
	return p.chanReady
}

func (p *testGRPCServerProvider) Aborted() error {
	return p.abortError
}
func (p *testGRPCServerProvider) ConsumedStderr() []string {
	return p.stdErrOutput
}
func (p *testGRPCServerProvider) ConsumedStdout() []string {
	return p.stdOutOutput
}
func (p *testGRPCServerProvider) Succeeded() bool {
	return p.success
}

// MustStartTestGRPCServer starts a test server and returns a client, a server and a server cleanup function.
// Fails test on any error.
func MustStartTestGRPCServer(t *testing.T, logger hclog.Logger, buildCtx *WorkContext) (TestServer, TestClient, func()) {
	grpcConfig := &GRPCServiceConfig{
		ServerName:        "test-grpc-server",
		BindHostPort:      "127.0.0.1:0",
		EmbeddedCAKeySize: 1024, // use this low for tests only! low value speeds up tests
	}
	testServer := NewTestServer(t, logger.Named("grpc-server"), grpcConfig, buildCtx)
	testServer.Start()
	select {
	case startErr := <-testServer.FailedNotify():
		t.Fatal("expected the GRPC server to start but it failed", startErr)
	case <-testServer.ReadyNotify():
		t.Log("GRPC server started and serving on", grpcConfig.BindHostPort)
	}
	testClient, clientErr := NewTestClient(t, logger.Named("grpc-client"), grpcConfig)
	if clientErr != nil {
		testServer.Stop()
		t.Fatal("expected the GRPC client, got error", clientErr)
	}
	return testServer, testClient, func() { testServer.Stop() }
}

// -- test client

type TestClient interface {
	Abort(error) error
	Commands(*testing.T) error
	NextCommand() commands.VMInitSerializableCommand
	Resource(string) (chan interface{}, error)
	StdErr([]string) error
	StdOut([]string) error
	Success() error
}

func NewTestClient(t *testing.T, logger hclog.Logger, cfg *GRPCServiceConfig) (TestClient, error) {
	grpcConn, err := grpc.Dial(cfg.BindHostPort,
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(cfg.MaxRecvMsgSize)),
		grpc.WithTransportCredentials(credentials.NewTLS(cfg.TLSConfigClient)))

	if err != nil {
		return nil, err
	}

	return &testClient{underlying: proto.NewRootfsServerClient(grpcConn)}, nil
}

type testClient struct {
	underlying      proto.RootfsServerClient
	fetchedCommands []commands.VMInitSerializableCommand
}

func (c *testClient) Commands(t *testing.T) error {
	c.fetchedCommands = []commands.VMInitSerializableCommand{}
	response, err := c.underlying.Commands(context.Background(), &proto.Empty{})
	if err != nil {
		return err
	}
	for _, cmd := range response.Command {
		rawItem := map[string]interface{}{}
		if err := json.Unmarshal([]byte(cmd), &rawItem); err != nil {
			return err
		}

		if originalCommandString, ok := rawItem["OriginalCommand"]; ok {
			if strings.HasPrefix(fmt.Sprintf("%s", originalCommandString), "ADD") {
				command := commands.Add{}
				if err := mapstructure.Decode(rawItem, &command); err != nil {
					return errors.Wrap(err, "found ADD but did not deserialize")
				}
				c.fetchedCommands = append(c.fetchedCommands, command)
			} else if strings.HasPrefix(fmt.Sprintf("%s", originalCommandString), "COPY") {
				command := commands.Copy{}
				if err := mapstructure.Decode(rawItem, &command); err != nil {
					return errors.Wrap(err, "found COPY but did not deserialize")
				}
				c.fetchedCommands = append(c.fetchedCommands, command)
			} else if strings.HasPrefix(fmt.Sprintf("%s", originalCommandString), "RUN") {
				command := commands.Run{}
				if err := mapstructure.Decode(rawItem, &command); err != nil {
					return errors.Wrap(err, "found RUN but did not deserialize")
				}
				c.fetchedCommands = append(c.fetchedCommands, command)
			} else {
				t.Log("unexpected command from grpc:", rawItem)
			}
		}
	}
	return nil
}

func (c *testClient) NextCommand() commands.VMInitSerializableCommand {
	if len(c.fetchedCommands) == 0 {
		return nil
	}
	result := c.fetchedCommands[0]
	if len(c.fetchedCommands) == 1 {
		c.fetchedCommands = []commands.VMInitSerializableCommand{}
	} else {
		c.fetchedCommands = c.fetchedCommands[1:]
	}
	return result
}

func (c *testClient) Resource(input string) (chan interface{}, error) {

	chanResources := make(chan interface{})

	resourceClient, err := c.underlying.Resource(context.Background(), &proto.ResourceRequest{Path: input})
	if err != nil {
		return nil, err
	}

	go func() {

		var currentResource *testResolvedResource

	out:
		for {
			response, err := resourceClient.Recv()

			if response == nil {
				resourceClient.CloseSend()
				break
			}

			// yes, err check after response check
			if err != nil {
				chanResources <- errors.Wrap(err, "failed reading chunk")
				break out
			}

			switch tresponse := response.GetPayload().(type) {
			case *proto.ResourceChunk_Eof:
				chanResources <- currentResource
			case *proto.ResourceChunk_Chunk:
				hash := sha256.Sum256(tresponse.Chunk.Chunk)
				if string(hash[:]) != string(tresponse.Chunk.Checksum) {
					chanResources <- errors.Wrap(err, "chunk checksum did not match")
					break out
				}
				currentResource.contents = append(currentResource.contents, tresponse.Chunk.Chunk...)
			case *proto.ResourceChunk_Header:
				currentResource = &testResolvedResource{
					contents:      []byte{},
					isDir:         tresponse.Header.IsDir,
					sourcePath:    tresponse.Header.SourcePath,
					targetMode:    fs.FileMode(tresponse.Header.FileMode),
					targetPath:    tresponse.Header.TargetPath,
					targetUser:    tresponse.Header.TargetUser,
					targetWorkdir: tresponse.Header.TargetWorkdir,
				}
			}
		}

		close(chanResources)

	}()

	return chanResources, nil
}

func (c *testClient) StdErr(input []string) error {
	_, err := c.underlying.StdErr(context.Background(), &proto.LogMessage{Line: input})
	return err
}
func (c *testClient) StdOut(input []string) error {
	_, err := c.underlying.StdOut(context.Background(), &proto.LogMessage{Line: input})
	return err
}
func (c *testClient) Abort(input error) error {
	_, err := c.underlying.Abort(context.Background(), &proto.AbortRequest{Error: input.Error()})
	return err
}
func (c *testClient) Success() error {
	_, err := c.underlying.Success(context.Background(), &proto.Empty{})
	return err
}

// --
// test resolved resource

type testResolvedResource struct {
	contents      []byte
	isDir         bool
	sourcePath    string
	targetMode    fs.FileMode
	targetPath    string
	targetUser    string
	targetWorkdir string
}

type bytesReaderCloser struct {
	bytesReader *bytes.Reader
}

func (b *bytesReaderCloser) Close() error {
	return nil
}

func (b *bytesReaderCloser) Read(p []byte) (n int, err error) {
	return b.bytesReader.Read(p)
}

func (r *testResolvedResource) Contents() (io.ReadCloser, error) {
	return &bytesReaderCloser{bytesReader: bytes.NewReader(r.contents)}, nil
}

func (r *testResolvedResource) IsDir() bool {
	return r.isDir
}

func (r *testResolvedResource) ResolvedURIOrPath() string {
	return fmt.Sprintf("grpc://%s", r.sourcePath)
}

func (r *testResolvedResource) SourcePath() string {
	return r.sourcePath
}
func (drr *testResolvedResource) TargetMode() fs.FileMode {
	return drr.targetMode
}
func (r *testResolvedResource) TargetPath() string {
	return r.targetPath
}
func (r *testResolvedResource) TargetWorkdir() commands.Workdir {
	return commands.Workdir{Value: r.targetWorkdir}
}
func (r *testResolvedResource) TargetUser() commands.User {
	return commands.User{Value: r.targetUser}
}
