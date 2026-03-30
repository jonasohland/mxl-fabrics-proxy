package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrInvalidArgument = errors.New("invalid argument")
)

type Service interface {
	Mux(*http.ServeMux)
}

type Message struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

type Server struct {
	wg       *sync.WaitGroup
	ctx      context.Context
	srv      http.Server
	mux      *http.ServeMux
	listener net.Listener
	services []Service
}

func NewServer(wg *sync.WaitGroup, ctx context.Context) *Server {
	server := &Server{
		wg:  wg,
		ctx: ctx,
		srv: http.Server{
			ReadTimeout:       time.Second * 3,
			ReadHeaderTimeout: time.Second * 3,
		},
		mux: http.NewServeMux(),
	}

	server.srv.Handler = server
	server.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		Error(w, ErrNotFound)
	})

	return server
}

func (s *Server) Mux(service Service) {
	service.Mux(s.mux)
	s.services = append(s.services, service)
}

func (s *Server) StartListening(address string, tlsc *tls.Config) error {
	if tlsc != nil {
		tlsl, err := tls.Listen("tcp", address, tlsc)
		if err != nil {
			return err
		}

		slog.Info("listening (tls)", "address", address)
		s.listener = tlsl
	} else {
		l, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}

		slog.Info("listening", "address", address)
		s.listener = l
	}

	s.wg.Add(2)
	go s.serve()
	go s.wait()

	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("recovered", "panic", p)
			Error(w, fmt.Errorf("%s", p))
		}
	}()

	s.mux.ServeHTTP(w, r)
}

func (s *Server) wait() {
	defer s.wg.Done()
	<-s.ctx.Done()

	shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	if err := s.srv.Shutdown(shutdownContext); err != nil {
		slog.Error("shutdown", "error", err)
	}
}

func (s *Server) serve() {
	defer s.wg.Done()
	if err := s.srv.Serve(s.listener); err != nil {
		slog.Error("serve", "error", err)
	}
}

func Error(w http.ResponseWriter, err error) {
	var message Message
	if errors.Is(err, ErrNotFound) {
		w.WriteHeader(http.StatusNotFound)
		message.Code = http.StatusNotFound
		message.Message = "not found"
	} else if errors.Is(err, ErrInvalidArgument) {
		w.WriteHeader(http.StatusBadRequest)
		message.Code = http.StatusBadRequest
		message.Message = err.Error()
	} else {
		w.WriteHeader(http.StatusInternalServerError)
		message.Code = http.StatusInternalServerError
		message.Message = err.Error()
	}

	_ = json.NewEncoder(w).Encode(&message)
}

func Response[T any](w http.ResponseWriter, t *T) {
	buf := bytes.NewBuffer(nil)
	_ = json.NewEncoder(buf).Encode(t)
	w.Header().Add("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}
