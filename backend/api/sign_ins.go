package api

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// Connecting a connector by signing in at its provider; see
// plans/SIGN_IN_CONNECTORS.md. Not built yet: each answers Unimplemented.

func (s *Service) StartConnectorSignIn(context.Context, *foundryv1.StartConnectorSignInRequest) (
	*foundryv1.ConnectorSignInResponse, error) {
	return nil, errSignInNotBuilt
}

func (s *Service) GetConnectorSignIn(context.Context, *foundryv1.GetConnectorSignInRequest) (
	*foundryv1.ConnectorSignInResponse, error) {
	return nil, errSignInNotBuilt
}

func (s *Service) AuthorizeConnectorSignIn(context.Context, *foundryv1.AuthorizeConnectorSignInRequest) (
	*foundryv1.AuthorizeConnectorSignInResponse, error) {
	return nil, errSignInNotBuilt
}

func (s *Service) FinishConnectorSignIn(context.Context, *foundryv1.FinishConnectorSignInRequest) (
	*foundryv1.ConnectorSignInResponse, error) {
	return nil, errSignInNotBuilt
}

var errSignInNotBuilt = status.Error(codes.Unimplemented, "connecting by signing in is not built yet")
