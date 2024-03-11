/*
Unless explicitly stated otherwise all files in this repository are licensed under the Apache-2.0 license.

This product includes software developed at Datadog (https://www.datadoghq.com/)
Copyright 2024 Datadog, Inc.
*/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	csilib "k8s.io/csi-translation-lib"
)

// AnnDynamicallyProvisioned annotation is added to a PV that has been dynamically provisioned by
// Kubernetes. Its value is name of volume plugin that created the volume.
// It serves both user (to show where a PV comes from) and Kubernetes (to
// recognize dynamically provisioned PVs in its decisions).
const AnnDynamicallyProvisioned = "pv.kubernetes.io/provisioned-by"

// AnnMigratedTo annotation is added to a PVC and PV that is supposed to be
// dynamically provisioned/deleted by its corresponding CSI driver
// through the CSIMigration feature flags. When this annotation is set the
// Kubernetes components will "stand-down" and the external-provisioner will
// act on the objects
const AnnMigratedTo = "pv.kubernetes.io/migrated-to"

// Each provisioner have a identify string to distinguish with others. This
// identify string will be added in PV annotations under this key.
const provisionerIDKey = "storage.kubernetes.io/csiProvisionerIdentity"

var (
	kubeconfig  = flag.String("kubeconfig", os.Getenv("HOME")+"/.kube/config", "kubeconfig file")
	kubecontext = flag.String("context", "", "name of the kube context to use to migrate")
	patchedApi  = flag.String("patched-api", "", "address of the patched api server")
	dryRun      = flag.Bool("dry-run", false, "dry run mode")
	logLevel    = zap.LevelFlag("log-level", zapcore.InfoLevel, "set log level")
	timeout     = flag.Duration("timeout", 5*time.Minute, "timeout for the migration")
	namespace   = flag.String("namespace", "", "namespace where migration should be done. If unset, all namespaces will be migrated")
	specificPV  = flag.String("pv", "", "only migrate a specific pv")
	specificPVC = flag.String("pvc", "", "only migrate the volume backing a specific pvc")
	rollback    = flag.Bool("rollback", false, "rollback migration")
	backupFile  = flag.String("f", "", "file where migrated volumes are backed up. In case we need to rollback")

	logger *zap.Logger
)

type volumeMarshaler struct {
	obj *v1.PersistentVolume
}

func (m volumeMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	bytes, err := k8sjson.Marshal(m.obj)
	if err != nil {
		return err
	}
	enc.AddString("pv", string(bytes))
	return nil
}

func main() {
	flag.Parse()

	zapConfig := zap.NewProductionConfig()
	zapConfig.Level = zap.NewAtomicLevelAt(*logLevel)
	zapConfig.Encoding = "console"
	zapConfig.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
	var err error // err cannot be declared inline because it will overwrite the global logger
	logger, err = zapConfig.Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: could not initialize looger: %q\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	if *rollback && *backupFile == "" {
		logger.Error("backup file must be specified with -f for rollback mode")
		os.Exit(1)
	}

	if *kubecontext == "" {
		logger.Error("missing required kubecontext flag")
		os.Exit(1)
	}

	if *patchedApi == "" {
		logger.Error("missing required patched-api flag")
		os.Exit(1)
	}

	err = migrate()
	if err != nil {
		logger.Fatal("error while migrating", zap.Error(err))
	}
	logger.Info("successfully migrated the volumes")
}

func migrate() error {
	ctx, cancel := context.WithTimeout(context.TODO(), *timeout)
	defer cancel()

	// Use the current context in kubeconfig
	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: *kubeconfig}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: *kubecontext}

	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		overrides,
	).ClientConfig()
	if err != nil {
		return err
	}

	config.Host = fmt.Sprintf("https://%s:443", *patchedApi)
	// Disable TLS verification since we may not have the correct certs on the patched server
	config.TLSClientConfig.Insecure = true
	config.TLSClientConfig.CAData = nil
	config.TLSClientConfig.CAFile = ""

	// Create the clientset
	patchedClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("error while building the patched api client: %w", err)
	}

	if *rollback {
		return rollbackMigration(ctx, patchedClient)
	}
	return pvsToCSI(ctx, patchedClient)
}

func rollbackMigration(ctx context.Context, apiClient *kubernetes.Clientset) error {
	rollbackedCounter := 0
	processedCounter := 0

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		volumes, err := readVolumesFile(*backupFile)
		if err != nil {
			return err
		}
		for _, volume := range volumes {
			volume.ResourceVersion = "" // clean resourceVersion to be able to update the pv with old version

			if !*dryRun {
				_, err := apiClient.CoreV1().PersistentVolumes().Update(ctx, &volume, metav1.UpdateOptions{})
				if err == nil {
					rollbackedCounter += 1
				} else {
					logger.Error(fmt.Errorf("could not rollback volume: %w", err).Error())
				}
			} else {
				logger.Info("would update volume")
				logger.Debug("updated volume", zap.Object("volume", volumeMarshaler{&volume}))
			}
			processedCounter += 1
		}
		logger.Info("rollbacked volumes", zap.Int("rollbacked", rollbackedCounter), zap.Int("processed", processedCounter))
		return nil
	})
}

func readVolumesFile(filename string) ([]v1.PersistentVolume, error) {
	var volumes []v1.PersistentVolume

	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	for {
		var pv v1.PersistentVolume
		if err := decoder.Decode(&pv); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		volumes = append(volumes, pv)
	}

	return volumes, nil
}

func pvsToCSI(ctx context.Context, apiClient *kubernetes.Clientset) error {
	migratedCounter := 0
	processedCounter := 0

	if _, err := os.Stat(*backupFile); err == nil { // file already exists
		dir, file := filepath.Split(*backupFile)
		newFile := fmt.Sprint(time.Now().Unix()) + "-" + file
		logger.Info("moving backup file", zap.String("old", file), zap.String("new", newFile))
		err := os.Rename(*backupFile, filepath.Join(dir, newFile))
		if err != nil {
			return err
		}
	}
	logger.Info("creating backup file", zap.String("file", *backupFile))
	file, err := os.Create(*backupFile)
	if err != nil {
		return fmt.Errorf("error opening file: %w", err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)

	// get Persistent Volumes
	listContinue := ""
	for {
		pvs, err := apiClient.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{Limit: 25, Continue: listContinue})
		if err != nil {
			return fmt.Errorf("could not list persistent volumes: %w", err)
		}

		for _, pv := range pvs.Items {
			logger := logger.With(zap.String("volumeName", pv.Name))
			processedCounter = processedCounter + 1

			// If set, only check specified pv
			if *specificPV != "" && pv.Name != *specificPV {
				logger.Debug("volume is not the one specified")
				continue
			}

			// If set, only check specified pvc's volume
			if *specificPVC != "" && pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Name != *specificPVC {
				logger.Debug("volume's pvc is not the one specified")
				continue
			}

			// If set, only check specified namespace
			if *namespace != "" && pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Namespace != *namespace {
				logger.Debug("volume pvc's is not in specified namespace")
				continue
			}

			// Already a CSI volume
			if pv.Spec.CSI != nil {
				logger.Info("volume already a CSI volume")
				continue
			}

			migrated, err := translateVolume(ctx, apiClient, pv, logger)
			if err != nil {
				logger.Error("failed to update the volume", zap.Error(err))
				continue
			}
			if migrated {
				err := encoder.Encode(pv)
				if err != nil {
					return fmt.Errorf("error encoding JSON: %w", err)
				}
				migratedCounter = migratedCounter + 1
			}
		}
		if pvs.Continue == "" {
			break // No more items, exit the loop
		}
		listContinue = pvs.Continue
	}
	logger.Info("migrated volumes", zap.Int("migrated", migratedCounter), zap.Int("processed", processedCounter))
	return nil
}

func translateVolume(ctx context.Context, apiClient *kubernetes.Clientset, pv v1.PersistentVolume, logger *zap.Logger) (bool, error) {
	migrated := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		volume, err := apiClient.CoreV1().PersistentVolumes().Get(ctx, pv.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("could not get volume: %w", err)
		}
		logger.Info("translating volume")

		// Check if in-tree PV can be translated to CSI PV
		csiTranslator := csilib.New()
		migratedTo := volume.Annotations[AnnMigratedTo]
		provisionedBy := volume.Annotations[AnnDynamicallyProvisioned]

		canTranslate := csiTranslator.IsMigratedCSIDriverByName(migratedTo) || csiTranslator.IsMigratedCSIDriverByName(provisionedBy)
		if !canTranslate {
			logger.Info("cannot find CSI PersistentVolumeSource for volume")
			return nil
		}

		// Attempt to translate in-tree PV to CSI PV
		volume, err = csiTranslator.TranslateInTreePVToCSI(volume)
		if err != nil {
			return fmt.Errorf("failed to translate in-tree PV: %w", err)
		}

		// Repair handle (this is only needed on GCP)
		nodeID, err := getNodeID(ctx, apiClient, migratedTo)
		if err != nil {
			return fmt.Errorf("could not get node id: %w", err)
		}
		volume.Spec.CSI.VolumeHandle, err = csiTranslator.RepairVolumeHandle(migratedTo, volume.Spec.CSI.VolumeHandle, nodeID)
		if err != nil {
			return fmt.Errorf("could not repair the volume handle: %w", err)
		}

		// Generate a provisioner identity, 0000 will represent our script
		timeStamp := time.Now().UnixNano() / int64(time.Millisecond)
		volume.Spec.CSI.VolumeAttributes[provisionerIDKey] = strconv.FormatInt(timeStamp, 10) + "-0000-" + volume.Spec.CSI.Driver

		// Remove element of the spec that are not used anymore with csi
		volume.Labels = map[string]string{}
		// Mark the volume as migrated
		volume.Labels["csimigrated"] = "true"

		newMatchExpressions := []v1.NodeSelectorRequirement{}
		for _, matchExpression := range volume.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions {
			if matchExpression.Key != "topology.kubernetes.io/region" {
				newMatchExpressions = append(newMatchExpressions, matchExpression)
			}
		}
		volume.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions = newMatchExpressions

		// Update annotations
		delete(volume.Annotations, AnnMigratedTo)
		volume.Annotations[AnnDynamicallyProvisioned] = volume.Spec.CSI.Driver

		// Azure csi volumes have some extra attributes that we need to recreate
		if volume.Spec.CSI.Driver == "disk.csi.azure.com" {
			volume.Spec.CSI.VolumeAttributes["csi.storage.k8s.io/pv/name"] = volume.Name
			volume.Spec.CSI.VolumeAttributes["csi.storage.k8s.io/pvc/name"] = volume.Spec.ClaimRef.Name
			volume.Spec.CSI.VolumeAttributes["csi.storage.k8s.io/pvc/namespace"] = volume.Spec.ClaimRef.Namespace
			requestedSize, ok := volume.Spec.Capacity.Storage().AsInt64()
			if !ok {
				return fmt.Errorf("could not convert storage size")
			}
			volume.Spec.CSI.VolumeAttributes["requestedsizegib"] = strconv.Itoa(int(requestedSize / (1024 * 1024 * 1024))) // convert bytes to GiB

			sc, err := apiClient.StorageV1().StorageClasses().Get(ctx, volume.Spec.StorageClassName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("could not get storage class: %w", err)
			}

			skuName, ok := sc.Parameters["skuName"]
			if !ok {
				return fmt.Errorf("could not find skuName in storage class %s", sc.Name)
			}
			volume.Spec.CSI.VolumeAttributes["skuName"] = skuName
		}

		if !*dryRun {
			_, err = apiClient.CoreV1().PersistentVolumes().Update(ctx, volume, metav1.UpdateOptions{})
			if err == nil {
				migrated = true
			}
		} else {
			migrated = true
			logger.Info("would update volume")
			logger.Debug("updated volume", zap.Object("volume", volumeMarshaler{volume}))
		}

		return err
	})
	return migrated, err
}

var nodeIDS = make(map[string]string)

func getNodeID(ctx context.Context, apiClient *kubernetes.Clientset, driver string) (string, error) {
	// Cache node ID
	if nodeID, ok := nodeIDS[driver]; ok {
		return nodeID, nil
	}

	csiNodes, err := apiClient.StorageV1().CSINodes().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil || len(csiNodes.Items) == 0 {
		return "", fmt.Errorf("couldn't get a node: %w", err)
	}

	drivers := csiNodes.Items[0].Spec.Drivers
	for _, d := range drivers {
		if d.Name == driver {
			nodeIDS[driver] = d.NodeID
			return d.NodeID, nil
		}
	}
	return "", fmt.Errorf("couldn't get node ID")
}
