package diff

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/hbagdi/deck/crud"
	"github.com/hbagdi/deck/state"
	"github.com/pkg/errors"
)

var (
	errEnqueueFailed = errors.New("failed to queue event")
)

// TODO get rid of the syncer struct and simply have a func for it

// Syncer takes in a current and target state of Kong,
// diffs them, generating a Graph to get Kong from current
// to target state.
type Syncer struct {
	currentState *state.KongState
	targetState  *state.KongState
	postProcess  crud.Registry

	eventChan chan Event
	errChan   chan error
	stopChan  chan struct{}

	InFlightOps int32

	SilenceWarnings bool

	once sync.Once
}

// NewSyncer constructs a Syncer.
func NewSyncer(current, target *state.KongState) (*Syncer, error) {
	s := &Syncer{}
	s.currentState, s.targetState = current, target

	s.postProcess.MustRegister("service", &servicePostAction{current})
	s.postProcess.MustRegister("route", &routePostAction{current})
	s.postProcess.MustRegister("upstream", &upstreamPostAction{current})
	s.postProcess.MustRegister("target", &targetPostAction{current})
	s.postProcess.MustRegister("certificate", &certificatePostAction{current})
	s.postProcess.MustRegister("ca_certificate", &caCertificatePostAction{current})
	s.postProcess.MustRegister("plugin", &pluginPostAction{current})
	s.postProcess.MustRegister("consumer", &consumerPostAction{current})
	s.postProcess.MustRegister("key-auth", &keyAuthPostAction{current})
	s.postProcess.MustRegister("hmac-auth", &hmacAuthPostAction{current})
	s.postProcess.MustRegister("jwt-auth", &jwtAuthPostAction{current})
	s.postProcess.MustRegister("basic-auth", &basicAuthPostAction{current})
	s.postProcess.MustRegister("acl-group", &aclGroupPostAction{current})
	s.postProcess.MustRegister("oauth2-cred", &oauth2CredPostAction{current})
	return s, nil
}

func (sc *Syncer) diff() error {
	var err error
	err = sc.createUpdate()
	if err != nil {
		return err
	}
	err = sc.delete()
	if err != nil {
		return err
	}
	return nil
}

func (sc *Syncer) delete() error {
	var err error
	err = sc.deletePlugins()
	if err != nil {
		return err
	}
	sc.wait()
	// routes should be deleted before services
	err = sc.deleteRoutes()
	if err != nil {
		return err
	}
	sc.wait()
	err = sc.deleteServices()
	if err != nil {
		return err
	}
	sc.wait()
	err = sc.deleteKeyAuths()
	if err != nil {
		return err
	}
	err = sc.deleteHMACAuths()
	if err != nil {
		return err
	}
	err = sc.deleteJWTAuths()
	if err != nil {
		return err
	}
	err = sc.deleteBasicAuths()
	if err != nil {
		return err
	}
	err = sc.deleteOauth2Creds()
	if err != nil {
		return err
	}
	err = sc.deleteACLGroups()
	if err != nil {
		return err
	}
	sc.wait()
	err = sc.deleteConsumers()
	if err != nil {
		return err
	}
	sc.wait()
	// targets should be deleted before upstreams
	// If an upstream is deleted, deleting a target will give back a 404.

	// TODO implement the following optimization:
	// If an upstream is deleted, do not make API calls to delete for it's
	// targets as they will be onCascade deleted in Kong, saving a few
	// round trip calls to Kong.
	err = sc.deleteTargets()
	if err != nil {
		return err
	}
	sc.wait()
	err = sc.deleteUpstreams()
	if err != nil {
		return err
	}
	err = sc.deleteCACertificates()
	if err != nil {
		return err
	}
	sc.wait()
	// TODO  Handle the following:
	// If a cert is changed but SNIs are the same,
	// the operation order will be to create the new cert and delete the old
	// cert. Creation will fail because the SNIs will still
	// be associated with the old cert.
	// This can be solved if SNI are also treated as a resource in this
	// codebase.
	err = sc.deleteCertificates()
	if err != nil {
		return err
	}
	sc.wait()
	return nil
}

func (sc *Syncer) createUpdate() error {
	// TODO write an interface and register by types,
	// then execute in a particular order

	// TODO optimize: increase parallelism
	// Unrelated entities like services, upstreams and certificates
	// can be all changed at the same time, then have a barrier
	// and then execute changes for routes, targets and snis.
	// services should be created before routes

	err := sc.createUpdateCertificates()
	if err != nil {
		return err
	}
	sc.wait()
	err = sc.createUpdateServices()
	if err != nil {
		return err
	}
	sc.wait()
	err = sc.createUpdateRoutes()
	if err != nil {
		return err
	}
	sc.wait()
	err = sc.createUpdateConsumers()
	if err != nil {
		return err
	}
	sc.wait()
	err = sc.createUpdateKeyAuths()
	if err != nil {
		return err
	}
	err = sc.createUpdateHMACAuths()
	if err != nil {
		return err
	}
	err = sc.createUpdateJWTAuths()
	if err != nil {
		return err
	}
	err = sc.createUpdateBasicAuths()
	if err != nil {
		return err
	}
	err = sc.createUpdateOauth2Creds()
	if err != nil {
		return err
	}
	err = sc.createUpdateACLGroups()
	if err != nil {
		return err
	}
	// TODO this barrier can be removed
	// TODO open up barriers and optimize
	sc.wait()
	// upstreams should be created before targets
	err = sc.createUpdateUpstreams()
	if err != nil {
		return err
	}
	sc.wait()
	err = sc.createUpdateTargets()
	if err != nil {
		return err
	}
	sc.wait()
	err = sc.createUpdatePlugins()
	if err != nil {
		return err
	}
	err = sc.createUpdateCACertificates()
	if err != nil {
		return err
	}
	sc.wait()
	return nil
}

func (sc *Syncer) queueEvent(e Event) error {
	atomic.AddInt32(&sc.InFlightOps, 1)
	select {
	case sc.eventChan <- e:
		return nil
	case <-sc.stopChan:
		return errEnqueueFailed
	}
}

func (sc *Syncer) eventCompleted(e Event) {
	atomic.AddInt32(&sc.InFlightOps, -1)
}

func (sc *Syncer) wait() {
	for atomic.LoadInt32(&sc.InFlightOps) != 0 {
		// TODO hack?
		time.Sleep(5 * time.Millisecond)
	}
}

// Run starts a diff and invokes d for every diff.
func (sc *Syncer) Run(done <-chan struct{}, parallelism int, d Do) []error {
	if parallelism < 1 {
		return append([]error{}, errors.New("parallelism can not be negative"))
	}

	var wg sync.WaitGroup

	sc.eventChan = make(chan Event, 10)
	sc.stopChan = make(chan struct{})
	sc.errChan = make(chan error)

	// run rabbit run
	// start the consumers
	wg.Add(parallelism)
	for i := 0; i < parallelism; i++ {
		go func(a int) {
			err := sc.eventLoop(d, a)
			if err != nil {
				sc.errChan <- err
			}
			wg.Done()
		}(i)
	}

	// start the producer
	wg.Add(1)
	go func() {
		err := sc.diff()
		if err != nil {
			sc.errChan <- err
		}
		close(sc.eventChan)
		wg.Done()
	}()

	// close the error chan once all done
	go func() {
		wg.Wait()
		close(sc.errChan)
	}()

	var errs []error
	select {
	case <-done:
	case err, ok := <-sc.errChan:
		if ok && err != nil {
			if err != errEnqueueFailed {
				errs = append(errs, err)
			}
		}
	}

	// stop the producer
	close(sc.stopChan)

	// collect errors
	for err := range sc.errChan {
		if err != errEnqueueFailed {

			errs = append(errs, err)
		}
	}

	return errs
}

// Do is the worker function to sync the diff
// TODO remove crud.Arg
type Do func(a Event) (crud.Arg, error)

func (sc *Syncer) eventLoop(d Do, a int) error {
	for event := range sc.eventChan {
		err := sc.handleEvent(d, event, a)
		sc.eventCompleted(event)
		if err != nil {
			return err
		}
	}
	return nil
}

func (sc *Syncer) handleEvent(d Do, event Event, a int) error {
	res, err := d(event)
	if err != nil {
		return errors.Wrapf(err, "while processing event")
	}
	if res == nil {
		return errors.New("result of event is nil")
	}
	_, err = sc.postProcess.Do(event.Kind, event.Op, res)
	if err != nil {
		return errors.Wrap(err, "while post processing event")
	}
	return nil
}
