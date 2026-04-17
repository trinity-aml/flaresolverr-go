package flaresolverr

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Service struct {
	cfg      Config
	logger   Logger
	sessions *sessionStore
	factory  browserFactory

	userAgentMu sync.Mutex
	userAgent   string
}

type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

func NewService(cfg Config) *Service {
	return newService(cfg, defaultBrowserFactory{})
}

func newService(cfg Config, factory browserFactory) *Service {
	cfg = cfg.withDefaults()
	service := &Service{
		cfg:     cfg,
		logger:  cfg.Logger,
		factory: factory,
	}
	service.sessions = newSessionStore(cfg, factory, service.peekUserAgent)
	return service
}

func (s *Service) Close() error {
	s.sessions.destroyAll()
	return nil
}

func (s *Service) Index(ctx context.Context) IndexResponse {
	return IndexResponse{
		Msg:       "FlareSolverr is ready!",
		Version:   s.cfg.Version,
		UserAgent: s.getUserAgent(ctx),
	}
}

func (s *Service) Health(context.Context) HealthResponse {
	return HealthResponse{Status: StatusOK}
}

func (s *Service) ControllerV1(ctx context.Context, req *V1Request) (V1Response, int) {
	start := time.Now().UnixMilli()

	res, err := s.handleV1(ctx, req)
	statusCode := 200
	if err != nil {
		res = V1Response{
			Status:  StatusError,
			Message: "Error: " + err.Error(),
		}
		statusCode = 500
		s.logger.Error("request failed", "err", err)
	}

	res.StartTimestamp = start
	res.EndTimestamp = time.Now().UnixMilli()
	res.Version = s.cfg.Version
	return res, statusCode
}

func (s *Service) handleV1(ctx context.Context, req *V1Request) (V1Response, error) {
	if req == nil {
		return V1Response{}, errors.New("Request body is mandatory.")
	}
	if req.Cmd == "" {
		return V1Response{}, errors.New("Request parameter 'cmd' is mandatory.")
	}
	if req.Headers != nil {
		s.logger.Warn("request parameter 'headers' was removed in FlareSolverr v2")
	}
	if req.UserAgent != "" {
		s.logger.Warn("request parameter 'userAgent' was removed in FlareSolverr v2")
	}
	if req.MaxTimeout < 1 {
		req.MaxTimeout = 60000
	}

	switch req.Cmd {
	case "sessions.create":
		return s.cmdSessionsCreate(ctx, req)
	case "sessions.list":
		return s.cmdSessionsList(), nil
	case "sessions.destroy":
		return s.cmdSessionsDestroy(req)
	case "request.get":
		return s.cmdRequest(ctx, req, "GET")
	case "request.post":
		return s.cmdRequest(ctx, req, "POST")
	default:
		return V1Response{}, fmt.Errorf("Request parameter 'cmd' = '%s' is invalid.", req.Cmd)
	}
}

func (s *Service) cmdSessionsCreate(ctx context.Context, req *V1Request) (V1Response, error) {
	proxy := req.Proxy
	if proxy == nil {
		proxy = s.cfg.DefaultProxy
	}
	item, fresh, err := s.sessions.create(req.Session, proxy, false)
	if err != nil {
		return V1Response{}, err
	}
	if !fresh {
		return V1Response{
			Status:  StatusOK,
			Message: "Session already exists.",
			Session: item.id,
		}, nil
	}
	s.storeUserAgentFromBrowser(ctx, item.browser)
	return V1Response{
		Status:  StatusOK,
		Message: "Session created successfully.",
		Session: item.id,
	}, nil
}

func (s *Service) cmdSessionsList() V1Response {
	return V1Response{
		Status:   StatusOK,
		Message:  "",
		Sessions: s.sessions.ids(),
	}
}

func (s *Service) cmdSessionsDestroy(req *V1Request) (V1Response, error) {
	if !s.sessions.destroy(req.Session) {
		return V1Response{}, errors.New("The session doesn't exist.")
	}
	return V1Response{
		Status:  StatusOK,
		Message: "The session has been removed.",
	}, nil
}

func (s *Service) cmdRequest(ctx context.Context, req *V1Request, method string) (V1Response, error) {
	if req.URL == "" {
		return V1Response{}, fmt.Errorf("Request parameter 'url' is mandatory in '%s' command.", req.Cmd)
	}
	if method == "GET" && req.PostData != "" {
		return V1Response{}, errors.New("Cannot use 'postBody' when sending a GET request.")
	}
	if method == "POST" && req.PostData == "" {
		return V1Response{}, errors.New("Request parameter 'postData' is mandatory in 'request.post' command.")
	}
	if req.Download != nil {
		s.logger.Warn("request parameter 'download' was removed in FlareSolverr v2")
	}
	if req.ReturnRawHTML != nil {
		s.logger.Warn("request parameter 'returnRawHtml' was removed in FlareSolverr v2")
	}

	result, message, err := s.resolveChallenge(ctx, req, method)
	if err != nil {
		return V1Response{}, err
	}
	return V1Response{
		Status:   StatusOK,
		Message:  message,
		Solution: result,
	}, nil
}

func (s *Service) resolveChallenge(ctx context.Context, req *V1Request, method string) (*ChallengeResolutionResult, string, error) {
	var (
		client browserClient
		item   *session
		err    error
	)

	if req.Session != "" {
		ttl := time.Duration(req.SessionTTLMinutes) * time.Minute
		item, _, err = s.sessions.get(req.Session, ttl)
		if err != nil {
			return nil, "", err
		}
		client = item.browser
	} else {
		cfg := s.runtimeBrowserConfig()
		proxy := req.Proxy
		if proxy == nil {
			proxy = cfg.DefaultProxy
		}
		client, err = s.factory.New(cfg, proxy)
		if err != nil {
			return nil, "", fmt.Errorf("create ephemeral browser: %w", err)
		}
		defer client.Close()
	}

	request := browserRequest{
		URL:               req.URL,
		Method:            method,
		PostData:          req.PostData,
		Cookies:           req.Cookies,
		MaxTimeoutMS:      req.MaxTimeout,
		ReturnOnlyCookies: req.ReturnOnlyCookies,
		ReturnScreenshot:  req.ReturnScreenshot,
		WaitInSeconds:     req.WaitInSeconds,
		DisableMedia:      req.DisableMedia != nil && *req.DisableMedia || req.DisableMedia == nil && s.cfg.DisableMedia,
		TabsTillVerify:    req.TabsTillVerify,
		LogHTML:           s.cfg.LogHTML,
	}

	if item != nil {
		item.mu.Lock()
		defer item.mu.Unlock()
	}

	res, err := client.Resolve(ctx, request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, "", fmt.Errorf("Error solving the challenge. Timeout after %s seconds.", formatTimeoutSeconds(req.MaxTimeout))
		}
		return nil, "", fmt.Errorf("Error solving the challenge. %s", stringsReplace(err.Error()))
	}
	if res != nil && res.Result != nil {
		s.storeUserAgent(res.Result.UserAgent)
	}
	return res.Result, res.Message, nil
}

func formatTimeoutSeconds(timeoutMS int) string {
	seconds := float64(timeoutMS)
	if seconds < 1 {
		seconds = 1
	}
	seconds /= 1000
	return strconv.FormatFloat(seconds, 'f', -1, 64)
}

func (s *Service) getUserAgent(ctx context.Context) string {
	_ = ctx
	return s.peekUserAgent()
}

func (s *Service) peekUserAgent() string {
	s.userAgentMu.Lock()
	defer s.userAgentMu.Unlock()
	return s.userAgent
}

func (s *Service) runtimeBrowserConfig() Config {
	cfg := s.cfg.withDefaults()
	cfg.StartupUserAgent = s.peekUserAgent()
	return cfg
}

func (s *Service) storeUserAgent(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	s.userAgentMu.Lock()
	if s.userAgent == "" {
		s.userAgent = value
	}
	s.userAgentMu.Unlock()
}

func (s *Service) storeUserAgentFromBrowser(ctx context.Context, client browserClient) {
	if client == nil || s.peekUserAgent() != "" {
		return
	}

	userAgent, err := client.UserAgent(ctx)
	if err != nil {
		s.logger.Debug("read browser user agent failed", "err", err)
		return
	}
	s.storeUserAgent(userAgent)
}

func stringsReplace(value string) string {
	return strings.NewReplacer("\n", "\\n", "\r", "").Replace(value)
}
