package omny

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

const every = 30 * time.Second

func Run(ctx context.Context, path string) error {
	c, err := Load(path)
	if err != nil {
		return err
	}
	s := serve(c)
	if err := s.pool.restore(); err != nil {
		// Do not block startup on state restore because counters can rebuild.
		s.log.Warn("state not restored, starting from zero", "path", c.State, "err", err)
	}
	n := 0
	for _, p := range c.Providers {
		n += len(p.Keys)
	}
	s.log.Info("listening", "addr", c.Listen, "providers", len(c.Providers), "keys", n)

	if c.Pid != "" {
		if err := os.WriteFile(c.Pid, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
			s.log.Warn("pidfile not written; omny add cannot reload this process", "path", c.Pid, "err", err)
		} else {
			defer os.Remove(c.Pid)
		}
	}

	stop := make(chan struct{})
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		s.pool.flush(stop, every, s.log)
	}()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer close(hup)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			if err := s.reload(path); err != nil {
				// Scrub reload errors because the decoder may echo a secret.
				s.log.Error("reload rejected, keeping the running config", "path", path,
					"err", s.pool.scrub(err.Error()))
				continue
			}
			s.log.Info("reloaded", "path", path)
		}
	}()

	srv := s.listener()
	shut := make(chan error, 1)
	go func() {
		<-ctx.Done()
		shut <- srv.Shutdown(context.Background())
	}()

	err = srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		err = <-shut
	}
	close(stop)
	// Wait for the final flush before returning so accepted usage is not lost.
	<-flushed
	return err
}
