package monitor

import (
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/openshift/baremetal-runtimecfg/pkg/config"
	"github.com/openshift/baremetal-runtimecfg/pkg/render"
	"github.com/sirupsen/logrus"
)

const (
	frrControlSock = "/var/run/frr/frr.sock"
)

func handleBootstrapStopFrr(kubeconfigPath string, bootstrapStopFrr chan APIState) {
	consecutiveErr := 0

	/* It could take up to ~20 seconds for the local kube-apiserver to start running on the bootstrap node,
	so before checking if kube-apiserver is not operational we should verify (with a timeout of 60 seconds)
	first that it's operational. */
	log.Info("handleBootstrapStopFrr: verify first that local kube-apiserver is operational")
	for start := time.Now(); time.Since(start) < time.Second*60; {
		if _, err := config.GetIngressConfig(kubeconfigPath, ""); err == nil {
			log.Info("handleBootstrapStopFrr: local kube-apiserver is operational")
			break
		}
		log.Info("handleBootstrapStopFrr: local kube-apiserver not operational")
		time.Sleep(3 * time.Second)
	}

	for {
		if _, err := config.GetIngressConfig(kubeconfigPath, ""); err != nil {
			log.Info("handleBootstrapStopFrr: detect failure on local kube-apiserver")

			// We have started to talk to Ironic through the API VIP as well,
			// so if Ironic is still up then we need to keep the VIP, even if
			// the apiserver has gone down.
			if _, err = http.Get("http://localhost:6385/v1"); err != nil {
				consecutiveErr++
				log.WithFields(logrus.Fields{
					"consecutiveErr": consecutiveErr,
				}).Info("handleBootstrapStopFrr: detect failure on Ironic (can be ignored if platform is not baremetal)")
			}
		} else {
			if consecutiveErr > bootstrapApiFailuresThreshold { // Means it was stopped
				bootstrapStopFrr <- started
			}
			consecutiveErr = 0
		}
		if consecutiveErr > bootstrapApiFailuresThreshold {
			log.WithFields(logrus.Fields{
				"consecutiveErr":                consecutiveErr,
				"bootstrapApiFailuresThreshold": bootstrapApiFailuresThreshold,
			}).Info("handleBootstrapStopFrr: Num of failures exceeds threshold")
			bootstrapStopFrr <- stopped
		}
		time.Sleep(1 * time.Second)
	}
}

func FrrWatch(kubeconfigPath, clusterConfigPath, templatePath, cfgPath string, apiVips, ingressVips []net.IP, interval time.Duration) error {
	var err error
	newConfig, err := config.GetConfig(kubeconfigPath, clusterConfigPath, "/etc/resolv.conf", apiVips, ingressVips, 0, 0, 0)
	if err != nil {
		return err
	}
	err = render.RenderFile(cfgPath, templatePath, newConfig)
	if err != nil {
		log.WithFields(logrus.Fields{
			"config": newConfig,
		}).Error("Failed to render Frr configuration")
		return err
	}
	signals := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	bootstrapStopFrr := make(chan APIState, 1)

	signal.Notify(signals, syscall.SIGTERM)
	signal.Notify(signals, syscall.SIGINT)
	go func() {
		<-signals
		done <- true
	}()

	if os.Getenv("IS_BOOTSTRAP") == "yes" {
		/* When OPENSHIFT_INSTALL_PRESERVE_BOOTSTRAP is set to true the bootstrap node won't be destroyed and
		   Frr on the bootstrap continue to run, this behavior will cause problems when the masters are up
		   with the VIP so, Frr on bootstrap should stop running when local kube-apiserver isn't operational anymore.
		   handleBootstrapStopFrr function is responsible to stop Frr when the condition is met. */
		go handleBootstrapStopFrr(kubeconfigPath, bootstrapStopFrr)
	}

	conn, err := net.Dial("unix", frrControlSock)
	if err != nil {
		return err
	}
	defer conn.Close()

	for {
		select {
		case <-done:
			return nil

		case APIStateChanged := <-bootstrapStopFrr:
			//Verify that stop message sent successfully
			for {
				var cmdMsg []byte
				if APIStateChanged == stopped {
					cmdMsg = []byte("stop\n")
				} else {
					cmdMsg = []byte("reload\n")
				}
				_, err := conn.Write(cmdMsg)
				if err == nil {
					log.Infof("Command message successfully sent to Frr container control socket: %s", string(cmdMsg[:]))
					break
				}
				log.WithFields(logrus.Fields{
					"socket": frrControlSock,
				}).Error("Failed to write command to Frr container control socket")
				time.Sleep(1 * time.Second)
			}
			// Make sure we don't send multiple messages in close succession if the
			// bootstrapStopFrr queue has more than one item in it.
			time.Sleep(5 * time.Second)

		default:
			time.Sleep(interval)
		}
	}
}
