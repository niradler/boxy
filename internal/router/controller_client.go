package router

import "boxy.dev/boxy/internal/controller"

type ControllerClient = controller.Client
type ControllerClientConfig = controller.ClientConfig
type ControllerHTTPError = controller.HTTPError
type CreateSandboxReq = controller.CreateSandboxReq
type ExecReq = controller.ExecReq
type ExecResult = controller.ExecResult
type DeleteSandboxReq = controller.DeleteSandboxReq

var (
	NewControllerClient = controller.NewClient
	IsStaleRouteError   = controller.IsStaleRouteError
)
