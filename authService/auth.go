package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	// Add this line

	"connectrpc.com/connect"
	"github.com/alexey-petrov/go-server/db"
	"github.com/alexey-petrov/go-server/jwtService"
	authv1 "github.com/alexey-petrov/go-webauthn/gen/auth/v1" // generated by protoc-gen-go
)

type User struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"firstName"`
	LastName  string `json:"lastName"`
}

func Auth(user User) (string, error) {
	var err error

	gormUser := db.User{}
	id, err := gormUser.CreateAdmin(user.Email, user.Password, user.FirstName, user.LastName)

	if err != nil {
		return "", err
	}

	token, err := jwtService.GenerateJWTPair(id)

	if err != nil {
		fmt.Println("Error generating JWT:", err)
		return "", err
	}

	return token, err
}

type AuthServiceServer struct{}

func (s *AuthServiceServer) Login(
	ctx context.Context,
	req *connect.Request[authv1.LoginRequest],
) (*connect.Response[authv1.LoginResponse], error) {
	user := db.User{}
	userData, err := user.LoginAsAdmin(req.Msg.Email, req.Msg.Password)

	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("unauth: failed to login. invalid credentials"))
	}

	jwtService.RevokeJWTByUserId(userData.UserId)

	token, err := jwtService.GenerateJWTPair(userData.UserId)

	if err != nil {
		fmt.Println("Error generating JWT:", err)
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New(err.Error()))
	}

	res := connect.NewResponse(&authv1.LoginResponse{
		AccessToken: token,
	})
	res.Header().Set("Login-Version", "v1")
	res.Header().Set("Set-Cookie", jwtService.GetConnectRpcAccessTokenCookie(token))
	return res, nil
}

func generateChallenge() ([]byte, error) {
	challenge := make([]byte, 32)
	_, err := rand.Read(challenge)
	return challenge, err
}

func (s *AuthServiceServer) BeginRegistration(
	ctx context.Context,
	req *connect.Request[authv1.BeginRegistrationRequest],
) (*connect.Response[authv1.BeginRegistrationResponse], error) {
	challenge, err := generateChallenge()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Respond with challenge and relying party (RP) ID
	resp := &authv1.BeginRegistrationResponse{
		Challenge: challenge,
		RpId:      "localhost", // Replace with your RP ID (your domain)
	}
	return connect.NewResponse(resp), nil
}

func (s *AuthServiceServer) FinishRegistration(
	ctx context.Context,
	req *connect.Request[authv1.FinishRegistrationRequest],
) (*connect.Response[authv1.FinishRegistrationResponse], error) {
	fmt.Println("FinishRegistration")
	fmt.Println(req.Msg.AttestationObject)

	credentialID := req.Msg.CredentialId
	attestationObject := req.Msg.AttestationObject
	// clientDataJSON := req.Msg.ClientDataJson

	// Here, you would validate the attestationObject and clientDataJSON
	// Extract the public key from the attestationObject (omitted for simplicity)
	// Step 1: Decode and parse the attestationObject
	authenticatorData, publicKey, err := parseAttestationObject(attestationObject)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid attestation object"))
	}
	// // Step 2: Validate the attestation (signature, certificate chain, etc.)
	// if err := validateAttestation(attestationObject, clientDataJSON, authenticatorData); err != nil {
	// 	return nil, connect.NewError(connect.CodeUnauthenticated, err)
	// }

	// Step 3: Store the user's public key and credentials in the database
	user := db.User{
		CredentialID:        []byte(credentialID),
		PublicKey:           publicKey,
		SignCount:           extractSignCount(authenticatorData), // Initial sign count
		AuthenticatorAAGUID: extractAAGUID(authenticatorData),    // AAGUID for the authenticator
	}

	id, err := user.CreateWebAuthnAdmin(&user)

	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	token, err := jwtService.GenerateJWTPair(id)

	if err != nil {
		fmt.Println("Error generating JWT:", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	fmt.Println(token)
	res := connect.NewResponse(&authv1.FinishRegistrationResponse{AccessToken: token})

	res.Header().Set("Set-Cookie", jwtService.GetConnectRpcAccessTokenCookie(token))

	return res, nil
}

func (s *AuthServiceServer) BeginLogin(
	ctx context.Context,
	req *connect.Request[authv1.BeginLoginRequest],
) (*connect.Response[authv1.BeginLoginResponse], error) {
	challenge, err := generateChallenge()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Respond with challenge and relying party (RP) ID
	resp := &authv1.BeginLoginResponse{
		Challenge: challenge,
		RpId:      "localhost", // Replace with your RP ID (your domain)
	}
	return connect.NewResponse(resp), nil
}

func (s *AuthServiceServer) FinishLogin(
	ctx context.Context,
	req *connect.Request[authv1.FinishLoginRequest],
) (*connect.Response[authv1.FinishLoginResponse], error) {
	// Parse the credential response from the client
	credentialID := req.Msg.CredentialId
	authenticatorData := req.Msg.AuthenticatorData
	clientDataJSON := req.Msg.ClientDataJson
	signature := req.Msg.Signature

	// Retrieve the user from the database using the Credential ID
	var user db.User
	if err := db.DBConn.Where("credential_id = ?", credentialID).First(&user).Error; err != nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("user not found"))
	}
	// Verify the signature using the stored public key
	isValid, err := verifySignature(user.PublicKey, authenticatorData, clientDataJSON, signature)
	if !isValid || err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid signature"))
	}
	// Ensure the sign count has increased (replay attack protection)
	// Does not work for passkeys/fingerprint
	// newSignCount := extractSignCount(authenticatorData)
	// fmt.Println(newSignCount, user.SignCount)

	// if newSignCount <= user.SignCount {
	// 	return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("replay attack detected"))
	// }

	_, err = user.LoginAsWebAuthAdmin(user.UserId)

	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	jwtService.RevokeJWTByUserId(user.UserId)

	token, err := jwtService.GenerateJWTPair(user.UserId)

	if err != nil {
		fmt.Println("Error generating JWT:", err)
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	res := connect.NewResponse(&authv1.FinishLoginResponse{AccessToken: token})

	res.Header().Set("Set-Cookie", jwtService.GetConnectRpcAccessTokenCookie(token))

	return res, nil
}
