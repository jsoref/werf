package manager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/werf/lockgate"
	"github.com/werf/logboek"
	"github.com/werf/logboek/pkg/style"
	"github.com/werf/logboek/pkg/types"
	"github.com/werf/werf/pkg/build/stage"
	"github.com/werf/werf/pkg/container_runtime"
	"github.com/werf/werf/pkg/image"
	"github.com/werf/werf/pkg/storage"
	"github.com/werf/werf/pkg/storage/lrumeta"
	"github.com/werf/werf/pkg/util/parallel"
	"github.com/werf/werf/pkg/werf"
	yaml "gopkg.in/yaml.v2"
)

var (
	ErrShouldResetStagesStorageCache = errors.New("should reset storage cache")
	ErrStageNotFound                 = errors.New("stage not found")
)

func IsStageNotFound(err error) bool {
	if err != nil {
		return strings.HasSuffix(err.Error(), ErrStageNotFound.Error())
	}
	return false
}

type ForEachDeleteStageOptions struct {
	storage.DeleteImageOptions
	storage.FilterStagesAndProcessRelatedDataOptions
}

type StorageManagerInterface interface {
	InitCache(ctx context.Context) error

	GetStagesStorage() storage.StagesStorage
	GetFinalStagesStorage() storage.StagesStorage
	GetSecondaryStagesStorageList() []storage.StagesStorage
	GetImageInfoGetter(imageName string, stg stage.Interface) *image.InfoGetter

	EnableParallel(parallelTasksLimit int)
	MaxNumberOfWorkers() int
	GenerateStageUniqueID(digest string, stages []*image.StageDescription) (string, int64)

	LockStageImage(ctx context.Context, imageName string) error
	AtomicStoreStagesByDigestToCache(ctx context.Context, stageName, stageDigest string, stageIDs []image.StageID) error
	GetStagesByDigest(ctx context.Context, stageName, stageDigest string) ([]*image.StageDescription, error)
	GetStagesByDigestFromStagesStorage(ctx context.Context, stageName, stageDigest string, stagesStorage storage.StagesStorage) ([]*image.StageDescription, error)
	GetStageDescriptionList(ctx context.Context) ([]*image.StageDescription, error)
	GetFinalStageDescriptionList(ctx context.Context) ([]*image.StageDescription, error)
	ResetStagesStorageCache(ctx context.Context) error

	FetchStage(ctx context.Context, containerRuntime container_runtime.ContainerRuntime, stg stage.Interface) error
	SelectSuitableStage(ctx context.Context, c stage.Conveyor, stg stage.Interface, stages []*image.StageDescription) (*image.StageDescription, error)
	CopySuitableByDigestStage(ctx context.Context, stageDesc *image.StageDescription, sourceStagesStorage, destinationStagesStorage storage.StagesStorage, containerRuntime container_runtime.ContainerRuntime) (*image.StageDescription, error)
	CopyStageIntoCache(ctx context.Context, stg stage.Interface, containerRuntime container_runtime.ContainerRuntime) error
	CopyStageIntoFinalRepo(ctx context.Context, stg stage.Interface, containerRuntime container_runtime.ContainerRuntime) error

	ForEachDeleteStage(ctx context.Context, options ForEachDeleteStageOptions, stagesDescriptions []*image.StageDescription, f func(ctx context.Context, stageDesc *image.StageDescription, err error) error) error
	ForEachDeleteFinalStage(ctx context.Context, options ForEachDeleteStageOptions, stagesDescriptions []*image.StageDescription, f func(ctx context.Context, stageDesc *image.StageDescription, err error) error) error
	ForEachRmImageMetadata(ctx context.Context, projectName, imageNameOrID string, stageIDCommitList map[string][]string, f func(ctx context.Context, commit, stageID string, err error) error) error
	ForEachRmManagedImage(ctx context.Context, projectName string, managedImages []string, f func(ctx context.Context, managedImage string, err error) error) error
	ForEachGetImportMetadata(ctx context.Context, projectName string, ids []string, f func(ctx context.Context, metadataID string, metadata *storage.ImportMetadata, err error) error) error
	ForEachRmImportMetadata(ctx context.Context, projectName string, ids []string, f func(ctx context.Context, id string, err error) error) error
}

func ShouldResetStagesStorageCache(err error) bool {
	if err != nil {
		return strings.HasSuffix(err.Error(), ErrShouldResetStagesStorageCache.Error())
	}
	return false
}

func NewStorageManager(projectName string, stagesStorage storage.StagesStorage, finalStagesStorage storage.StagesStorage, secondaryStagesStorageList []storage.StagesStorage, cacheStagesStorageList []storage.StagesStorage, storageLockManager storage.LockManager, stagesStorageCache storage.StagesStorageCache) *StorageManager {
	return &StorageManager{
		ProjectName:        projectName,
		StorageLockManager: storageLockManager,
		StagesStorageCache: stagesStorageCache,

		StagesStorage:              stagesStorage,
		FinalStagesStorage:         finalStagesStorage,
		CacheStagesStorageList:     cacheStagesStorageList,
		SecondaryStagesStorageList: secondaryStagesStorageList,
	}
}

type StagesList struct {
	Mux      sync.Mutex
	StageIDs []image.StageID
}

func NewStagesList(stageIDs []image.StageID) *StagesList {
	return &StagesList{
		StageIDs: stageIDs,
	}
}

func (stages *StagesList) GetStageIDs() []image.StageID {
	stages.Mux.Lock()
	defer stages.Mux.Unlock()

	var res []image.StageID

	for _, stg := range stages.StageIDs {
		res = append(res, stg)
	}

	return res
}

func (stages *StagesList) AddStageID(stageID image.StageID) {
	stages.Mux.Lock()
	defer stages.Mux.Unlock()

	for _, stg := range stages.StageIDs {
		if stg.IsEqual(stageID) {
			return
		}
	}

	stages.StageIDs = append(stages.StageIDs, stageID)
}

type StorageManager struct {
	parallel           bool
	parallelTasksLimit int

	ProjectName string

	StorageLockManager storage.LockManager
	StagesStorageCache storage.StagesStorageCache

	StagesStorage              storage.StagesStorage
	FinalStagesStorage         storage.StagesStorage
	CacheStagesStorageList     []storage.StagesStorage
	SecondaryStagesStorageList []storage.StagesStorage

	// These will be released automatically when current process exits
	SharedHostImagesLocks []lockgate.LockHandle

	FinalStagesListCacheMux sync.Mutex
	FinalStagesListCache    *StagesList
}

func (m *StorageManager) GetStagesStorage() storage.StagesStorage {
	return m.StagesStorage
}

func (m *StorageManager) GetFinalStagesStorage() storage.StagesStorage {
	return m.FinalStagesStorage
}

func (m *StorageManager) GetSecondaryStagesStorageList() []storage.StagesStorage {
	return m.SecondaryStagesStorageList
}

func (m *StorageManager) GetImageInfoGetter(imageName string, stg stage.Interface) *image.InfoGetter {
	stageID := stg.GetImage().GetStageDescription().StageID
	info := stg.GetImage().GetStageDescription().Info

	if m.FinalStagesStorage != nil {
		finalImageName := m.FinalStagesStorage.ConstructStageImageName(m.ProjectName, stageID.Digest, stageID.UniqueID)
		_, tag := image.ParseRepositoryAndTag(finalImageName)
		return image.NewInfoGetter(imageName, finalImageName, tag)
	}

	return image.NewInfoGetter(
		imageName,
		info.Name,
		info.Tag,
	)
}

func (m *StorageManager) InitCache(ctx context.Context) error {
	logboek.Context(ctx).Info().LogF("Initializing storage manager cache\n")

	if m.FinalStagesStorage != nil {
		if _, err := m.getOrCreateFinalStagesListCache(ctx); err != nil {
			return fmt.Errorf("unable to get or create final stages list cache: %s", err)
		}
	}

	return nil
}

func (m *StorageManager) EnableParallel(parallelTasksLimit int) {
	m.parallel = true
	m.parallelTasksLimit = parallelTasksLimit
}

func (m *StorageManager) MaxNumberOfWorkers() int {
	if m.parallel && m.parallelTasksLimit > 0 {
		return m.parallelTasksLimit
	}

	return 1
}

func (m *StorageManager) ResetStagesStorageCache(ctx context.Context) error {
	msg := fmt.Sprintf("Reset storage cache %s for project %q", m.StagesStorageCache.String(), m.ProjectName)
	return logboek.Context(ctx).Default().LogProcess(msg).DoError(func() error {
		return m.StagesStorageCache.DeleteAllStages(ctx, m.ProjectName)
	})
}

func (m *StorageManager) GetStageDescriptionList(ctx context.Context) ([]*image.StageDescription, error) {
	var stageIDs []image.StageID

	found, stageIDs, err := m.StagesStorageCache.GetAllStages(ctx, m.ProjectName)
	if err != nil {
		return nil, fmt.Errorf("error getting stages ids from stages storage cache: %s", err)
	}
	if !found {
		stageIDs, err = m.StagesStorage.GetStagesIDs(ctx, m.ProjectName)
		if err != nil {
			return nil, fmt.Errorf("error getting stages ids from %s: %s", m.StagesStorage, err)
		}
	}

	var mutex sync.Mutex
	var stages []*image.StageDescription

	if err := parallel.DoTasks(ctx, len(stageIDs), parallel.DoTasksOptions{
		MaxNumberOfWorkers: m.MaxNumberOfWorkers(),
	}, func(ctx context.Context, taskId int) error {
		stageID := stageIDs[taskId]

		if stageDesc, err := getStageDescription(ctx, m.ProjectName, stageID, m.StagesStorage, m.CacheStagesStorageList, getStageDescriptionOptions{AllowStagesStorageCacheReset: true, WithLocalManifestCache: m.getWithLocalManifestCacheOption()}); err != nil {
			return fmt.Errorf("error getting stage %s description: %s", stageID.String(), err)
		} else if stageDesc == nil {
			logboek.Context(ctx).Warn().LogF("Ignoring stage %s: cannot get stage description from %s\n", stageID.String(), m.StagesStorage.String())
		} else {
			mutex.Lock()
			defer mutex.Unlock()

			stages = append(stages, stageDesc)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return stages, nil
}

func (m *StorageManager) GetFinalStageDescriptionList(ctx context.Context) ([]*image.StageDescription, error) {
	existingStagesListCache, err := m.getOrCreateFinalStagesListCache(ctx)
	if err != nil {
		return nil, fmt.Errorf("error getting existing stages list of final repo %s: %s", m.FinalStagesStorage.String(), err)
	}

	logboek.Context(ctx).Debug().LogF("[%p] Got existing final stages list cache: %#v\n", m, existingStagesListCache.StageIDs)

	stageIDs := existingStagesListCache.GetStageIDs()

	var mutex sync.Mutex
	var stages []*image.StageDescription

	if err := parallel.DoTasks(ctx, len(stageIDs), parallel.DoTasksOptions{
		MaxNumberOfWorkers: m.MaxNumberOfWorkers(),
	}, func(ctx context.Context, taskId int) error {
		stageID := stageIDs[taskId]

		if stageDesc, err := getStageDescription(ctx, m.ProjectName, stageID, m.FinalStagesStorage, nil, getStageDescriptionOptions{AllowStagesStorageCacheReset: true, WithLocalManifestCache: true}); err != nil {
			return fmt.Errorf("error getting stage %s description from %s: %s", stageID.String(), m.FinalStagesStorage.String(), err)
		} else if stageDesc == nil {
			logboek.Context(ctx).Warn().LogF("Ignoring stage %s: cannot get stage description from %s\n", stageID.String(), m.FinalStagesStorage.String())
		} else {
			mutex.Lock()
			defer mutex.Unlock()

			stages = append(stages, stageDesc)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return stages, nil
}

func (m *StorageManager) ForEachDeleteFinalStage(ctx context.Context, options ForEachDeleteStageOptions, stagesDescriptions []*image.StageDescription, f func(ctx context.Context, stageDesc *image.StageDescription, err error) error) error {
	return parallel.DoTasks(ctx, len(stagesDescriptions), parallel.DoTasksOptions{
		MaxNumberOfWorkers:         m.MaxNumberOfWorkers(),
		InitDockerCLIForEachWorker: true,
	}, func(ctx context.Context, taskId int) error {
		stageDescription := stagesDescriptions[taskId]

		err := m.FinalStagesStorage.DeleteStage(ctx, stageDescription, options.DeleteImageOptions)
		return f(ctx, stageDescription, err)
	})
}

func (m *StorageManager) ForEachDeleteStage(ctx context.Context, options ForEachDeleteStageOptions, stagesDescriptions []*image.StageDescription, f func(ctx context.Context, stageDesc *image.StageDescription, err error) error) error {
	if localStagesStorage, isLocal := m.StagesStorage.(*storage.LocalDockerServerStagesStorage); isLocal {
		filteredStagesDescriptions, err := localStagesStorage.FilterStagesAndProcessRelatedData(ctx, stagesDescriptions, options.FilterStagesAndProcessRelatedDataOptions)
		if err != nil {
			return fmt.Errorf("error filtering local docker server stages: %s", err)
		}

		stagesDescriptions = filteredStagesDescriptions
	}

	for _, stageDesc := range stagesDescriptions {
		if err := m.StagesStorageCache.DeleteStagesByDigest(ctx, m.ProjectName, stageDesc.StageID.Digest); err != nil {
			return fmt.Errorf("unable to delete storage cache record (%s): %s", stageDesc.StageID.Digest, err)
		}
	}

	return parallel.DoTasks(ctx, len(stagesDescriptions), parallel.DoTasksOptions{
		MaxNumberOfWorkers:         m.MaxNumberOfWorkers(),
		InitDockerCLIForEachWorker: true,
	}, func(ctx context.Context, taskId int) error {
		stageDescription := stagesDescriptions[taskId]

		for _, cacheStagesStorage := range m.CacheStagesStorageList {
			if err := cacheStagesStorage.DeleteStage(ctx, stageDescription, options.DeleteImageOptions); err != nil {
				logboek.Context(ctx).Warn().LogF("Unable to delete stage %s from the cache stages storage %s: %s\n", stageDescription.StageID.String(), cacheStagesStorage.String(), err)
			}
		}

		err := m.StagesStorage.DeleteStage(ctx, stageDescription, options.DeleteImageOptions)
		return f(ctx, stageDescription, err)
	})
}

func (m *StorageManager) LockStageImage(ctx context.Context, imageName string) error {
	imageLockName := container_runtime.ImageLockName(imageName)

	_, lock, err := werf.AcquireHostLock(ctx, imageLockName, lockgate.AcquireOptions{Shared: true})
	if err != nil {
		return fmt.Errorf("error locking %q shared lock: %s", imageLockName, err)
	}

	m.SharedHostImagesLocks = append(m.SharedHostImagesLocks, lock)

	return nil
}

func doFetchStage(ctx context.Context, projectName string, stagesStorage storage.StagesStorage, stageID image.StageID, dockerImage *container_runtime.DockerImage) error {
	err := logboek.Context(ctx).Info().LogProcess("Check manifest availability").DoError(func() error {
		freshStageDescription, err := stagesStorage.GetStageDescription(ctx, projectName, stageID.Digest, stageID.UniqueID)
		if err != nil {
			return fmt.Errorf("unable to get stage description: %s", err)
		}

		if freshStageDescription == nil {
			return ErrStageNotFound
		}

		dockerImage.Image.SetStageDescription(freshStageDescription)

		return nil
	})
	if err != nil {
		return err
	}

	return logboek.Context(ctx).Info().LogProcess("Fetch image").DoError(func() error {
		logboek.Context(ctx).Debug().LogF("Image name: %s\n", dockerImage.Image.Name())

		if err := stagesStorage.FetchImage(ctx, dockerImage); err != nil {
			return fmt.Errorf("unable to fetch stage %s image %s: %s", stageID.String(), dockerImage.Image.Name(), err)
		}
		return nil
	})
}

func copyStageIntoStagesStorage(ctx context.Context, projectName string, stageID image.StageID, dockerImage *container_runtime.DockerImage, stagesStorage storage.StagesStorage, containerRuntime container_runtime.ContainerRuntime) error {
	targetStagesStorageImageName := stagesStorage.ConstructStageImageName(projectName, stageID.Digest, stageID.UniqueID)

	if err := containerRuntime.RenameImage(ctx, dockerImage, targetStagesStorageImageName, false); err != nil {
		return fmt.Errorf("unable to rename image %s to %s: %s", dockerImage.Image.Name(), targetStagesStorageImageName, err)
	}

	if err := stagesStorage.StoreImage(ctx, dockerImage); err != nil {
		return fmt.Errorf("unable to store stage %s into the cache stages storage %s: %s", stageID.String(), stagesStorage.String(), err)
	}

	if err := storeStageDescriptionIntoLocalManifestCache(ctx, projectName, stageID, stagesStorage, convertStageDescriptionForStagesStorage(dockerImage.Image.GetStageDescription(), stagesStorage)); err != nil {
		return fmt.Errorf("error storing stage %s description into local manifest cache: %s", targetStagesStorageImageName, err)
	}

	if err := lrumeta.CommonLRUImagesCache.AccessImage(ctx, targetStagesStorageImageName); err != nil {
		return fmt.Errorf("error accessing last recently used images cache for %s: %s", targetStagesStorageImageName, err)
	}

	return nil
}

func (m *StorageManager) FetchStage(ctx context.Context, containerRuntime container_runtime.ContainerRuntime, stg stage.Interface) error {
	logboek.Context(ctx).Debug().LogF("-- StagesManager.FetchStage %s\n", stg.LogDetailedName())

	if err := m.LockStageImage(ctx, stg.GetImage().Name()); err != nil {
		return fmt.Errorf("error locking stage image %q: %s", stg.GetImage().Name(), err)
	}

	shouldFetch, err := m.StagesStorage.ShouldFetchImage(ctx, &container_runtime.DockerImage{Image: stg.GetImage()})
	if err != nil {
		return fmt.Errorf("error checking should fetch image: %s", err)
	}
	if !shouldFetch {
		imageName := m.StagesStorage.ConstructStageImageName(m.ProjectName, stg.GetImage().GetStageDescription().StageID.Digest, stg.GetImage().GetStageDescription().StageID.UniqueID)

		logboek.Context(ctx).Info().LogF("Image %s exists, will not perform fetch\n", imageName)

		if err := lrumeta.CommonLRUImagesCache.AccessImage(ctx, imageName); err != nil {
			return fmt.Errorf("error accessing last recently used images cache for %s: %s", imageName, err)
		}

		return nil
	}

	var fetchedDockerImage *container_runtime.DockerImage
	var cacheStagesStorageListToRefill []storage.StagesStorage

	fetchStageFromCache := func(stagesStorage storage.StagesStorage) (*container_runtime.DockerImage, error) {
		stageID := stg.GetImage().GetStageDescription().StageID
		imageName := stagesStorage.ConstructStageImageName(m.ProjectName, stageID.Digest, stageID.UniqueID)
		stageImage := container_runtime.NewStageImage(nil, imageName, containerRuntime.(*container_runtime.LocalDockerServerRuntime))
		dockerImage := &container_runtime.DockerImage{Image: stageImage}

		shouldFetch, err := stagesStorage.ShouldFetchImage(ctx, dockerImage)
		if err != nil {
			return nil, fmt.Errorf("error checking should fetch image from cache repo %s: %s", stagesStorage.String(), err)
		}

		if shouldFetch {
			logboek.Context(ctx).Info().LogF("Cache repo image %s does not exist locally, will perform fetch\n", dockerImage.Image.Name())

			proc := logboek.Context(ctx).Default().LogProcess("Fetching stage %s from %s", stg.LogDetailedName(), stagesStorage.String())
			proc.Start()

			err := doFetchStage(ctx, m.ProjectName, stagesStorage, *stageID, dockerImage)

			if IsStageNotFound(err) {
				logboek.Context(ctx).Default().LogF("Stage not found\n")
				proc.End()
				return nil, err
			}

			if err != nil {
				proc.Fail()
				return nil, err
			}

			proc.End()

			if err := storeStageDescriptionIntoLocalManifestCache(ctx, m.ProjectName, *stageID, stagesStorage, dockerImage.Image.GetStageDescription()); err != nil {
				return nil, fmt.Errorf("error storing stage %s description into local manifest cache: %s", imageName, err)
			}

		} else {
			logboek.Context(ctx).Info().LogF("Cache repo image %s exists locally, will not perform fetch\n", dockerImage.Image.Name())
		}

		if err := lrumeta.CommonLRUImagesCache.AccessImage(ctx, dockerImage.Image.Name()); err != nil {
			return nil, fmt.Errorf("error accessing last recently used images cache for %s: %s", dockerImage.Image.Name(), err)
		}

		return dockerImage, nil
	}

	prepareCacheStageAsPrimary := func(cacheDockerImage *container_runtime.DockerImage, primaryStage stage.Interface) error {
		stageID := primaryStage.GetImage().GetStageDescription().StageID
		primaryImageName := m.StagesStorage.ConstructStageImageName(m.ProjectName, stageID.Digest, stageID.UniqueID)

		if err := containerRuntime.RenameImage(ctx, cacheDockerImage, primaryImageName, false); err != nil {
			return fmt.Errorf("unable to rename image %s to %s: %s", fetchedDockerImage.Image.Name(), primaryImageName, err)
		}

		if err := containerRuntime.RefreshImageObject(ctx, &container_runtime.DockerImage{Image: primaryStage.GetImage()}); err != nil {
			return fmt.Errorf("unable to refresh stage image %s: %s", primaryStage.GetImage().Name(), err)
		}

		if err := storeStageDescriptionIntoLocalManifestCache(ctx, m.ProjectName, *stageID, m.StagesStorage, convertStageDescriptionForStagesStorage(cacheDockerImage.Image.GetStageDescription(), m.StagesStorage)); err != nil {
			return fmt.Errorf("error storing stage %s description into local manifest cache: %s", primaryImageName, err)
		}

		if err := lrumeta.CommonLRUImagesCache.AccessImage(ctx, primaryImageName); err != nil {
			return fmt.Errorf("error accessing last recently used images cache for %s: %s", primaryImageName, err)
		}

		return nil
	}

	for _, cacheStagesStorage := range m.CacheStagesStorageList {
		cacheDockerImage, err := fetchStageFromCache(cacheStagesStorage)
		if err != nil {
			if !IsStageNotFound(err) {
				logboek.Context(ctx).Warn().LogF("Unable to fetch stage %s from cache stages storage %s: %s\n", stg.GetImage().GetStageDescription().StageID.String(), cacheStagesStorage.String(), err)
			}

			cacheStagesStorageListToRefill = append(cacheStagesStorageListToRefill, cacheStagesStorage)

			continue
		}

		if err := prepareCacheStageAsPrimary(cacheDockerImage, stg); err != nil {
			logboek.Context(ctx).Warn().LogF("Unable to prepare stage %s fetched from cache stages storage %s as a primary: %s\n", cacheDockerImage.Image.Name(), cacheStagesStorage.String(), err)

			cacheStagesStorageListToRefill = append(cacheStagesStorageListToRefill, cacheStagesStorage)

			continue
		}

		fetchedDockerImage = cacheDockerImage
		break
	}

	if fetchedDockerImage == nil {
		stageID := stg.GetImage().GetStageDescription().StageID
		dockerImage := &container_runtime.DockerImage{Image: stg.GetImage()}

		err := logboek.Context(ctx).Default().LogProcess("Fetching stage %s from %s", stg.LogDetailedName(), m.StagesStorage.String()).
			DoError(func() error {
				return doFetchStage(ctx, m.ProjectName, m.StagesStorage, *stageID, dockerImage)
			})

		if err == ErrStageNotFound {
			logboek.Context(ctx).Error().LogF("Invalid stage %s image %q! Stage is no longer available in the %s. Stages storage cache for project %q should be reset!\n", stg.LogDetailedName(), stg.GetImage().Name(), m.StagesStorage.String(), m.ProjectName)
			return ErrShouldResetStagesStorageCache
		}

		if err == storage.ErrBrokenImage {
			logboek.Context(ctx).Error().LogF("Invalid stage %s image %q! Stage image is broken and is no longer available in the %s. Stages storage cache for project %q should be reset!\n", stg.LogDetailedName(), stg.GetImage().Name(), m.StagesStorage.String(), m.ProjectName)

			logboek.Context(ctx).Error().LogF("Will mark image %q as rejected in the stages storage %s\n", stg.GetImage().Name(), m.StagesStorage.String())
			if err := m.StagesStorage.RejectStage(ctx, m.ProjectName, stageID.Digest, stageID.UniqueID); err != nil {
				return fmt.Errorf("unable to reject stage %s image %s in the stages storage %s: %s", stg.LogDetailedName(), stg.GetImage().Name(), m.StagesStorage.String(), err)
			}

			return ErrShouldResetStagesStorageCache
		}

		if err != nil {
			return fmt.Errorf("unable to fetch stage %s from stages storage %s: %s", stageID.String(), m.StagesStorage.String(), err)
		}

		fetchedDockerImage = dockerImage
	}

	for _, cacheStagesStorage := range cacheStagesStorageListToRefill {
		stageID := stg.GetImage().GetStageDescription().StageID

		err := logboek.Context(ctx).Default().LogProcess("Copy stage %s into cache %s", stg.LogDetailedName(), cacheStagesStorage.String()).
			DoError(func() error {
				if err := copyStageIntoStagesStorage(ctx, m.ProjectName, *stageID, fetchedDockerImage, cacheStagesStorage, containerRuntime); err != nil {
					return fmt.Errorf("unable to copy stage %s into cache stages storage %s: %s", stageID.String(), cacheStagesStorage.String(), err)
				}
				return nil
			})
		if err != nil {
			logboek.Context(ctx).Warn().LogF("Warning %s\n", err)
		}
	}

	return nil
}

func (m *StorageManager) CopyStageIntoCache(ctx context.Context, stg stage.Interface, containerRuntime container_runtime.ContainerRuntime) error {
	for _, cacheStagesStorage := range m.CacheStagesStorageList {
		stageID := stg.GetImage().GetStageDescription().StageID
		dockerImage := &container_runtime.DockerImage{Image: stg.GetImage()}

		err := logboek.Context(ctx).Default().LogProcess("Copy stage %s into cache %s", stg.LogDetailedName(), cacheStagesStorage.String()).
			DoError(func() error {
				if err := copyStageIntoStagesStorage(ctx, m.ProjectName, *stageID, dockerImage, cacheStagesStorage, containerRuntime); err != nil {
					return fmt.Errorf("unable to copy stage %s into cache stages storage %s: %s", stageID.String(), cacheStagesStorage.String(), err)
				}
				return nil
			})
		if err != nil {
			logboek.Context(ctx).Warn().LogF("Warning: %s\n", err)
		}
	}

	return nil
}

func (m *StorageManager) getOrCreateFinalStagesListCache(ctx context.Context) (*StagesList, error) {
	m.FinalStagesListCacheMux.Lock()
	defer m.FinalStagesListCacheMux.Unlock()

	if m.FinalStagesListCache != nil {
		return m.FinalStagesListCache, nil
	}

	stageIDs, err := m.FinalStagesStorage.GetStagesIDs(ctx, m.ProjectName)
	if err != nil {
		return nil, fmt.Errorf("unable to get final repo stages list: %s", err)
	}
	m.FinalStagesListCache = NewStagesList(stageIDs)

	return m.FinalStagesListCache, nil
}

func (m *StorageManager) CopyStageIntoFinalRepo(ctx context.Context, stg stage.Interface, containerRuntime container_runtime.ContainerRuntime) error {
	existingStagesListCache, err := m.getOrCreateFinalStagesListCache(ctx)
	if err != nil {
		return fmt.Errorf("error getting existing stages list of final repo %s: %s", m.FinalStagesStorage.String(), err)
	}

	logboek.Context(ctx).Debug().LogF("[%p] Got existing final stages list cache: %#v\n", m, existingStagesListCache.StageIDs)

	stageID := stg.GetImage().GetStageDescription().StageID
	finalImageName := m.FinalStagesStorage.ConstructStageImageName(m.ProjectName, stageID.Digest, stageID.UniqueID)

	for _, existingStg := range existingStagesListCache.GetStageIDs() {
		if existingStg.IsEqual(*stageID) {
			logboek.Context(ctx).Info().LogF("Stage %s already exists in the final repo, skipping\n", stageID.String())

			logboek.Context(ctx).Default().LogFHighlight("Use cache final image for %s\n", stg.LogDetailedName())
			container_runtime.LogImageName(ctx, finalImageName)

			return nil
		}
	}

	if err := m.FetchStage(ctx, containerRuntime, stg); err != nil {
		return fmt.Errorf("unable to fetch stage %s: %s", stg.LogDetailedName(), err)
	}

	dockerImage := &container_runtime.DockerImage{Image: stg.GetImage()}

	err = logboek.Context(ctx).Default().LogProcess("Copy stage %s into the final repo", stg.LogDetailedName()).
		Options(func(options types.LogProcessOptionsInterface) {
			options.Style(style.Highlight())
		}).
		DoError(func() error {
			if err := copyStageIntoStagesStorage(ctx, m.ProjectName, *stageID, dockerImage, m.FinalStagesStorage, containerRuntime); err != nil {
				return fmt.Errorf("unable to copy stage %s into the final repo %s: %s", stageID.String(), m.FinalStagesStorage.String(), err)
			}

			logboek.Context(ctx).Default().LogFDetails("  name: %s\n", finalImageName)
			return nil
		})

	if err != nil {
		return err
	}

	existingStagesListCache.AddStageID(*stageID)

	logboek.Context(ctx).Debug().LogF("Updated existing final stages list: %#v\n", m.FinalStagesListCache.StageIDs)

	return nil
}

func (m *StorageManager) SelectSuitableStage(ctx context.Context, c stage.Conveyor, stg stage.Interface, stages []*image.StageDescription) (*image.StageDescription, error) {
	if len(stages) == 0 {
		return nil, nil
	}

	var stageDesc *image.StageDescription
	if err := logboek.Context(ctx).Info().LogProcess("Selecting suitable image for stage %s by digest %s", stg.Name(), stg.GetDigest()).
		DoError(func() error {
			var err error
			stageDesc, err = stg.SelectSuitableStage(ctx, c, stages)
			return err
		}); err != nil {
		return nil, err
	}
	if stageDesc == nil {
		return nil, nil
	}

	imgInfoData, err := yaml.Marshal(stageDesc)
	if err != nil {
		panic(err)
	}

	logboek.Context(ctx).Debug().LogBlock("Selected cache image").
		Options(func(options types.LogBlockOptionsInterface) {
			options.Style(style.Highlight())
		}).
		Do(func() {
			logboek.Context(ctx).Debug().LogF(string(imgInfoData))
		})

	return stageDesc, nil
}

func (m *StorageManager) AtomicStoreStagesByDigestToCache(ctx context.Context, stageName, stageDigest string, stageIDs []image.StageID) error {
	if lock, err := m.StorageLockManager.LockStageCache(ctx, m.ProjectName, stageDigest); err != nil {
		return fmt.Errorf("error locking stage %s cache by digest %s: %s", stageName, stageDigest, err)
	} else {
		defer m.StorageLockManager.Unlock(ctx, lock)
	}

	return logboek.Context(ctx).Info().LogProcess("Storing stage %s images by digest %s into storage cache", stageName, stageDigest).
		DoError(func() error {
			if err := m.StagesStorageCache.StoreStagesByDigest(ctx, m.ProjectName, stageDigest, stageIDs); err != nil {
				return fmt.Errorf("error storing stage %s images by digest %s into storage cache: %s", stageName, stageDigest, err)
			}
			return nil
		})
}

func (m *StorageManager) GetStagesByDigest(ctx context.Context, stageName, stageDigest string) ([]*image.StageDescription, error) {
	cacheExists, cacheStages, err := m.getStagesByDigestFromCache(ctx, stageName, stageDigest)
	if err != nil {
		return nil, fmt.Errorf("unable to get %s stages by digest %q from cache: %s", stageName, stageDigest, err)
	}
	if cacheExists {
		return cacheStages, nil
	}

	logboek.Context(ctx).Debug().LogF(
		"Stage %s cache by digest %s is not exist in the stages storage cache: will request fresh stages from storage and set stages storage cache by digest %s\n",
		stageName, stageDigest, stageDigest,
	)
	return m.atomicGetStagesByDigestWithStagesStorageCacheStore(ctx, stageName, stageDigest)
}

func (m *StorageManager) GetStagesByDigestFromStagesStorage(ctx context.Context, stageName, stageDigest string, stagesStorage storage.StagesStorage) ([]*image.StageDescription, error) {
	stageIDs, err := m.getStagesIDsByDigestFromStagesStorage(ctx, stageName, stageDigest, stagesStorage)
	if err != nil {
		return nil, fmt.Errorf("unable to get stages ids from %s by digest %s for stage %s: %s", stagesStorage.String(), stageDigest, stageName, err)
	}

	stages, err := m.getStagesDescriptions(ctx, stageIDs, stagesStorage, m.CacheStagesStorageList)
	if err != nil {
		return nil, fmt.Errorf("unable to get stage descriptions by ids from %s: %s", stagesStorage.String(), err)
	}

	return stages, nil
}

func (m *StorageManager) CopySuitableByDigestStage(ctx context.Context, stageDesc *image.StageDescription, sourceStagesStorage, destinationStagesStorage storage.StagesStorage, containerRuntime container_runtime.ContainerRuntime) (*image.StageDescription, error) {
	img := container_runtime.NewStageImage(nil, stageDesc.Info.Name, containerRuntime.(*container_runtime.LocalDockerServerRuntime))

	logboek.Context(ctx).Info().LogF("Fetching %s\n", img.Name())
	if err := sourceStagesStorage.FetchImage(ctx, &container_runtime.DockerImage{Image: img}); err != nil {
		return nil, fmt.Errorf("unable to fetch %s from %s: %s", stageDesc.Info.Name, sourceStagesStorage.String(), err)
	}

	newImageName := destinationStagesStorage.ConstructStageImageName(m.ProjectName, stageDesc.StageID.Digest, stageDesc.StageID.UniqueID)
	logboek.Context(ctx).Info().LogF("Renaming image %s to %s\n", img.Name(), newImageName)
	if err := containerRuntime.RenameImage(ctx, &container_runtime.DockerImage{Image: img}, newImageName, false); err != nil {
		return nil, err
	}

	logboek.Context(ctx).Info().LogF("Storing %s\n", newImageName)
	if err := destinationStagesStorage.StoreImage(ctx, &container_runtime.DockerImage{Image: img}); err != nil {
		return nil, fmt.Errorf("unable to store %s to %s: %s", stageDesc.Info.Name, destinationStagesStorage.String(), err)
	}

	if destinationStageDesc, err := getStageDescription(ctx, m.ProjectName, *stageDesc.StageID, destinationStagesStorage, m.CacheStagesStorageList, getStageDescriptionOptions{AllowStagesStorageCacheReset: true, WithLocalManifestCache: m.getWithLocalManifestCacheOption()}); err != nil {
		return nil, fmt.Errorf("unable to get stage %s description from %s: %s", stageDesc.StageID.String(), destinationStagesStorage.String(), err)
	} else {
		return destinationStageDesc, nil
	}
}

func (m *StorageManager) getWithLocalManifestCacheOption() bool {
	return m.StagesStorage.Address() != storage.LocalStorageAddress
}

func (m *StorageManager) getStagesByDigestFromCache(ctx context.Context, stageName, stageDigest string) (bool, []*image.StageDescription, error) {
	var cacheExists bool
	var cacheStagesIDs []image.StageID

	err := logboek.Context(ctx).Info().LogProcess("Getting stage %s images by digest %s from storage cache", stageName, stageDigest).
		DoError(func() error {
			var err error
			cacheExists, cacheStagesIDs, err = m.StagesStorageCache.GetStagesByDigest(ctx, m.ProjectName, stageDigest)
			if err != nil {
				return fmt.Errorf("error getting project %s stage %s images from storage cache: %s", m.ProjectName, stageDigest, err)
			}
			return nil
		})

	var stages []*image.StageDescription

	for _, stageID := range cacheStagesIDs {
		if stageDesc, err := getStageDescription(ctx, m.ProjectName, stageID, m.StagesStorage, m.CacheStagesStorageList, getStageDescriptionOptions{AllowStagesStorageCacheReset: true, WithLocalManifestCache: m.getWithLocalManifestCacheOption()}); err != nil {
			return false, nil, fmt.Errorf("unable to get stage %q description: %s", stageID.String(), err)
		} else {
			stages = append(stages, stageDesc)
		}
	}

	return cacheExists, stages, err
}

func (m *StorageManager) getStagesIDsByDigestFromStagesStorage(ctx context.Context, stageName, stageDigest string, stagesStorage storage.StagesStorage) ([]image.StageID, error) {
	var stageIDs []image.StageID
	if err := logboek.Context(ctx).Info().LogProcess("Get %s stages by digest %s from storage", stageName, stageDigest).
		DoError(func() error {
			var err error
			stageIDs, err = stagesStorage.GetStagesIDsByDigest(ctx, m.ProjectName, stageDigest)
			if err != nil {
				return fmt.Errorf("error getting project %s stage %s images from storage: %s", m.StagesStorage.String(), stageDigest, err)
			}

			logboek.Context(ctx).Debug().LogF("Stages ids: %#v\n", stageIDs)

			return nil
		}); err != nil {
		return nil, err
	}

	return stageIDs, nil
}

func (m *StorageManager) getStagesDescriptions(ctx context.Context, stageIDs []image.StageID, stagesStorage storage.StagesStorage, cacheStagesStorageList []storage.StagesStorage) ([]*image.StageDescription, error) {
	var stages []*image.StageDescription
	for _, stageID := range stageIDs {
		if stageDesc, err := getStageDescription(ctx, m.ProjectName, stageID, stagesStorage, cacheStagesStorageList, getStageDescriptionOptions{AllowStagesStorageCacheReset: false, WithLocalManifestCache: m.getWithLocalManifestCacheOption()}); err != nil {
			return nil, err
		} else if stageDesc == nil {
			logboek.Context(ctx).Warn().LogF("Ignoring stage %s: cannot get stage description from %s\n", stageID.String(), m.StagesStorage.String())
			continue
		} else {
			stages = append(stages, stageDesc)
		}
	}

	return stages, nil
}

func (m *StorageManager) atomicGetStagesByDigestWithStagesStorageCacheStore(ctx context.Context, stageName, stageDigest string) ([]*image.StageDescription, error) {
	if lock, err := m.StorageLockManager.LockStageCache(ctx, m.ProjectName, stageDigest); err != nil {
		return nil, fmt.Errorf("error locking project %s stage %s cache: %s", m.ProjectName, stageDigest, err)
	} else {
		defer m.StorageLockManager.Unlock(ctx, lock)
	}

	stageIDs, err := m.getStagesIDsByDigestFromStagesStorage(ctx, stageName, stageDigest, m.StagesStorage)
	if err != nil {
		return nil, fmt.Errorf("unable to get stages ids from %s by digest %s for stage %s: %s", m.StagesStorage.String(), stageDigest, stageName, err)
	}

	validStages, err := m.getStagesDescriptions(ctx, stageIDs, m.StagesStorage, m.CacheStagesStorageList)
	if err != nil {
		return nil, fmt.Errorf("unable to get stage descriptions by ids from %s: %s", m.StagesStorage.String(), err)
	}

	var validStageIDs []image.StageID
	for _, stage := range validStages {
		validStageIDs = append(validStageIDs, *stage.StageID)
	}

	if err := logboek.Context(ctx).Info().LogProcess("Storing %s stages by digest %s into stages storage cache", stageName, stageDigest).
		DoError(func() error {
			if err := m.StagesStorageCache.StoreStagesByDigest(ctx, m.ProjectName, stageDigest, validStageIDs); err != nil {
				return fmt.Errorf("error storing stage %s images by digest %s into storage cache: %s", stageName, stageDigest, err)
			}
			return nil
		}); err != nil {
		return nil, err
	}

	return validStages, nil
}

type getStageDescriptionOptions struct {
	AllowStagesStorageCacheReset bool
	WithLocalManifestCache       bool
}

func getStageDescriptionFromLocalManifestCache(ctx context.Context, projectName string, stageID image.StageID, stagesStorage storage.StagesStorage) (*image.StageDescription, error) {
	stageImageName := stagesStorage.ConstructStageImageName(projectName, stageID.Digest, stageID.UniqueID)

	logboek.Context(ctx).Debug().LogF("Getting image %s info from the manifest cache...\n", stageImageName)
	if imgInfo, err := image.CommonManifestCache.GetImageInfo(ctx, stagesStorage.String(), stageImageName); err != nil {
		return nil, fmt.Errorf("error getting image %s info: %s", stageImageName, err)
	} else if imgInfo != nil {
		logboek.Context(ctx).Info().LogF("Got image %s info from the manifest cache (CACHE HIT)\n", stageImageName)
		return &image.StageDescription{
			StageID: &image.StageID{Digest: stageID.Digest, UniqueID: stageID.UniqueID},
			Info:    imgInfo,
		}, nil
	} else {
		logboek.Context(ctx).Info().LogF("Not found %s image info in the manifest cache (CACHE MISS)\n", stageImageName)
	}

	return nil, nil
}

func storeStageDescriptionIntoLocalManifestCache(ctx context.Context, projectName string, stageID image.StageID, stagesStorage storage.StagesStorage, stageDesc *image.StageDescription) error {
	stageImageName := stagesStorage.ConstructStageImageName(projectName, stageID.Digest, stageID.UniqueID)

	logboek.Context(ctx).Debug().LogF("Storing image %s info into manifest cache\n", stageImageName)
	if err := image.CommonManifestCache.StoreImageInfo(ctx, stagesStorage.String(), stageDesc.Info); err != nil {
		return fmt.Errorf("error storing image %s info: %s", stageImageName, err)
	}

	return nil
}

func convertStageDescriptionForStagesStorage(stageDesc *image.StageDescription, stagesStorage storage.StagesStorage) *image.StageDescription {
	return &image.StageDescription{
		StageID: &image.StageID{
			Digest:   stageDesc.StageID.Digest,
			UniqueID: stageDesc.StageID.UniqueID,
		},
		Info: &image.Info{
			Name:              fmt.Sprintf("%s:%s-%d", stagesStorage.Address(), stageDesc.StageID.Digest, stageDesc.StageID.UniqueID),
			Repository:        stagesStorage.Address(),
			Tag:               stageDesc.Info.Tag,
			RepoDigest:        stageDesc.Info.RepoDigest,
			ID:                stageDesc.Info.ID,
			ParentID:          stageDesc.Info.ParentID,
			Labels:            stageDesc.Info.Labels,
			Size:              stageDesc.Info.Size,
			CreatedAtUnixNano: stageDesc.Info.CreatedAtUnixNano,
		},
	}
}

func getStageDescription(ctx context.Context, projectName string, stageID image.StageID, stagesStorage storage.StagesStorage, cacheStagesStorageList []storage.StagesStorage, opts getStageDescriptionOptions) (*image.StageDescription, error) {
	if opts.WithLocalManifestCache {
		stageDesc, err := getStageDescriptionFromLocalManifestCache(ctx, projectName, stageID, stagesStorage)
		if err != nil {
			return nil, fmt.Errorf("error getting stage %s description from %s: %s", stageID.String(), stagesStorage.String(), err)
		}
		if stageDesc != nil {
			return stageDesc, nil
		}
	}

	for _, cacheStagesStorage := range cacheStagesStorageList {
		if opts.WithLocalManifestCache {
			stageDesc, err := getStageDescriptionFromLocalManifestCache(ctx, projectName, stageID, cacheStagesStorage)
			if err != nil {
				return nil, fmt.Errorf("error getting stage %s description from the local manifest cache: %s", stageID.String(), err)
			}
			if stageDesc != nil {
				return convertStageDescriptionForStagesStorage(stageDesc, stagesStorage), nil
			}
		}

		var stageDesc *image.StageDescription
		err := logboek.Context(ctx).Info().LogProcess("Get stage %s description from cache stages storage %s", stageID.String(), cacheStagesStorage.String()).
			DoError(func() error {
				var err error
				stageDesc, err = cacheStagesStorage.GetStageDescription(ctx, projectName, stageID.Digest, stageID.UniqueID)

				logboek.Context(ctx).Debug().LogF("Got stage description: %#v\n", stageDesc)
				return err
			})
		if err != nil {
			logboek.Context(ctx).Warn().LogF("Unable to get stage description from cache stages storage %s: %s\n", cacheStagesStorage.String(), err)
			continue
		}

		if stageDesc != nil {
			if opts.WithLocalManifestCache {
				if err := storeStageDescriptionIntoLocalManifestCache(ctx, projectName, stageID, cacheStagesStorage, stageDesc); err != nil {
					return nil, fmt.Errorf("error storing stage %s description into local manifest cache: %s", stageID.String(), err)
				}
			}

			return convertStageDescriptionForStagesStorage(stageDesc, stagesStorage), nil
		}
	}

	logboek.Context(ctx).Debug().LogF("Getting digest %q uniqueID %d stage info from %s...\n", stageID.Digest, stageID.UniqueID, stagesStorage.String())
	if stageDesc, err := stagesStorage.GetStageDescription(ctx, projectName, stageID.Digest, stageID.UniqueID); err == storage.ErrBrokenImage {
		if opts.AllowStagesStorageCacheReset {
			stageImageName := stagesStorage.ConstructStageImageName(projectName, stageID.Digest, stageID.UniqueID)

			logboek.Context(ctx).Error().LogF("Invalid stage image %q! Stage is broken and is no longer available in the %s. Stages storage cache for project %q should be reset!\n", stageImageName, stagesStorage.String(), projectName)

			logboek.Context(ctx).Error().LogF("Will mark image %q as rejected in the stages storage %s\n", stageImageName, stagesStorage.String())
			if err := stagesStorage.RejectStage(ctx, projectName, stageID.Digest, stageID.UniqueID); err != nil {
				return nil, fmt.Errorf("unable to reject stage %s image %s in the stages storage %s: %s", stageID.String(), stageImageName, stagesStorage.String(), err)
			}

			return nil, ErrShouldResetStagesStorageCache
		}

		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("error getting digest %q uniqueID %d stage info from %s: %s", stageID.Digest, stageID.UniqueID, stagesStorage.String(), err)
	} else if stageDesc != nil {
		if opts.WithLocalManifestCache {
			if err := storeStageDescriptionIntoLocalManifestCache(ctx, projectName, stageID, stagesStorage, stageDesc); err != nil {
				return nil, fmt.Errorf("error storing stage %s description into local manifest cache: %s", stageID.String(), err)
			}
		}

		return stageDesc, nil
	} else if opts.AllowStagesStorageCacheReset {
		stageImageName := stagesStorage.ConstructStageImageName(projectName, stageID.Digest, stageID.UniqueID)

		logboek.Context(ctx).Error().LogF("Invalid stage image %q! Stage is no longer available in the %s. Storage cache for project %q should be reset!\n", stageImageName, stagesStorage.String(), projectName)

		return nil, ErrShouldResetStagesStorageCache
	} else {
		return nil, nil
	}
}

func (m *StorageManager) GenerateStageUniqueID(digest string, stages []*image.StageDescription) (string, int64) {
	var imageName string

	for {
		timeNow := time.Now().UTC()
		uniqueID := timeNow.Unix()*1000 + int64(timeNow.Nanosecond()/1000000)
		imageName = m.StagesStorage.ConstructStageImageName(m.ProjectName, digest, uniqueID)

		for _, stageDesc := range stages {
			if stageDesc.Info.Name == imageName {
				continue
			}
		}
		return imageName, uniqueID
	}
}

type rmImageMetadataTask struct {
	commit  string
	stageID string
}

func (m *StorageManager) ForEachRmImageMetadata(ctx context.Context, projectName, imageNameOrID string, stageIDCommitList map[string][]string, f func(ctx context.Context, commit, stageID string, err error) error) error {
	var tasks []rmImageMetadataTask
	for stageID, commitList := range stageIDCommitList {
		for _, commit := range commitList {
			tasks = append(tasks, rmImageMetadataTask{
				commit:  commit,
				stageID: stageID,
			})
		}
	}

	return parallel.DoTasks(ctx, len(tasks), parallel.DoTasksOptions{
		MaxNumberOfWorkers: m.MaxNumberOfWorkers(),
	}, func(ctx context.Context, taskId int) error {
		task := tasks[taskId]
		err := m.StagesStorage.RmImageMetadata(ctx, projectName, imageNameOrID, task.commit, task.stageID)
		return f(ctx, task.commit, task.stageID, err)
	})
}

func (m *StorageManager) ForEachRmManagedImage(ctx context.Context, projectName string, managedImages []string, f func(ctx context.Context, managedImage string, err error) error) error {
	return parallel.DoTasks(ctx, len(managedImages), parallel.DoTasksOptions{
		MaxNumberOfWorkers: m.MaxNumberOfWorkers(),
	}, func(ctx context.Context, taskId int) error {
		managedImage := managedImages[taskId]
		err := m.StagesStorage.RmManagedImage(ctx, projectName, managedImage)
		return f(ctx, managedImage, err)
	})
}

func (m *StorageManager) ForEachGetImportMetadata(ctx context.Context, projectName string, ids []string, f func(ctx context.Context, metadataID string, metadata *storage.ImportMetadata, err error) error) error {
	return parallel.DoTasks(ctx, len(ids), parallel.DoTasksOptions{
		MaxNumberOfWorkers: m.MaxNumberOfWorkers(),
	}, func(ctx context.Context, taskId int) error {
		id := ids[taskId]
		metadata, err := m.StagesStorage.GetImportMetadata(ctx, projectName, id)
		return f(ctx, id, metadata, err)
	})
}

func (m *StorageManager) ForEachRmImportMetadata(ctx context.Context, projectName string, ids []string, f func(ctx context.Context, id string, err error) error) error {
	return parallel.DoTasks(ctx, len(ids), parallel.DoTasksOptions{
		MaxNumberOfWorkers: m.MaxNumberOfWorkers(),
	}, func(ctx context.Context, taskId int) error {
		id := ids[taskId]
		err := m.StagesStorage.RmImportMetadata(ctx, projectName, id)
		return f(ctx, id, err)
	})
}
