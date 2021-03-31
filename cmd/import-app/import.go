package main

import (
	"context"
	"errors"
	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	v1alpha1 "kubesphere.io/openpitrix-jobs/pkg/apis/application/v1alpha1"
	typedv1alpha1 "kubesphere.io/openpitrix-jobs/pkg/client/clientset/versioned/typed/application/v1alpha1"
	"kubesphere.io/openpitrix-jobs/pkg/constants"
	"kubesphere.io/openpitrix-jobs/pkg/idutils"
	"kubesphere.io/openpitrix-jobs/pkg/s3"
	"os"
	"path"
)

var builtinKey = "application.kubesphere.io/builtin-app"
var chartDir string
var (
	InvalidScheme = errors.New("invalid scheme")
)

func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "import app",
		Run: func(cmd *cobra.Command, args []string) {
			s3Client, err := s3.NewS3Client(s3Options)
			if err != nil {

			}
			wf := &ImportWorkFlow{
				client:   versionedClient.ApplicationV1alpha1(),
				s3Cleint: s3Client,
			}

			file, err := os.Open(chartDir)
			if err != nil {
				klog.Fatalf("failed opening directory: %s, error: %s", chartDir, err)
			}
			defer file.Close()

			fileList, err := file.Readdir(0)
			if err != nil {
				klog.Fatalf("read dir failed, error: %s", err)
			}

			for _, fileInfo := range fileList {
				if fileInfo.IsDir() {
					continue
				}

				chrt, err := loader.LoadFile(path.Join(chartDir, fileInfo.Name()))
				if err != nil {
					klog.Fatalf("load chart data failed failed, error: %s", err)
				}

				app, err := wf.CreateApp(context.TODO(), chrt)
				if err != nil {
					klog.Fatalf("create chart %s failed, error: %s", chrt.Name(), err)
				}

				appVer, err := wf.CreateAppVer(context.TODO(), app, path.Join(chartDir, fileInfo.Name()))
				if err != nil {
					klog.Errorf("create app version failed, error: %s", err)
				}
				_ = appVer
			}
		},
	}

	f := cmd.Flags()

	f.StringVar(&chartDir, "chart-dir",
		"/root/package",
		"the dir to which charts are saved")

	return cmd
}

type ImportWorkFlow struct {
	client   typedv1alpha1.ApplicationV1alpha1Interface
	s3Cleint s3.Interface
}

var _ importInterface = &ImportWorkFlow{}

type importInterface interface {
	CreateApp(ctx context.Context, chrt *chart.Chart) (*v1alpha1.HelmApplication, error)
	CreateAppVer(ctx context.Context, app *v1alpha1.HelmApplication, chartFileName string) (*v1alpha1.HelmApplicationVersion, error)
	UpdateAppVersionStatus(ctx context.Context, appVer *v1alpha1.HelmApplicationVersion) (*v1alpha1.HelmApplicationVersion, error)
}

func (wf *ImportWorkFlow) CreateApp(ctx context.Context, chrt *chart.Chart) (app *v1alpha1.HelmApplication, err error) {
	appList, err := wf.client.HelmApplications().List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{builtinKey: "true"}).String(),
	})

	if err != nil {
		klog.Errorf("get application list failed, error: %s", err)
		return nil, err
	}

	for ind := range appList.Items {
		item := &appList.Items[ind]
		if item.GetTrueName() == chrt.Name() {
			klog.Infof("helm application exists, id: %s", item.Name)
			return item, nil
		}
	}

	appId := idutils.GetUuid36(v1alpha1.HelmApplicationIdPrefix)

	// create helm application
	app = &v1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name: appId,
			Labels: map[string]string{
				builtinKey: "true",
			},
			Annotations: map[string]string{
				constants.CreatorAnnotationKey: "admin",
			},
		},
		Spec: v1alpha1.HelmApplicationSpec{
			Name:        chrt.Name(),
			Description: chrt.Metadata.Description,
			Icon:        chrt.Metadata.Icon,
		},
	}

	return wf.client.HelmApplications().Create(ctx, app, metav1.CreateOptions{})
}

func (wf *ImportWorkFlow) CreateAppVer(ctx context.Context, app *v1alpha1.HelmApplication, chartFileName string) (*v1alpha1.HelmApplicationVersion, error) {

	chrt, err := loader.LoadFile(chartFileName)

	if err != nil {
		klog.Fatalf("load chart data failed failed, error: %s", err)
		return nil, err
	}

	chartFile, _ := os.Open(chartFileName)

	appVerList, err := wf.client.HelmApplicationVersions().List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{constants.ChartApplicationIdLabelKey: app.Name}).String(),
	})

	if err != nil {
		klog.Errorf("get application version list failed, error: %s", err)
		return nil, err
	}

	var existsAppVer *v1alpha1.HelmApplicationVersion

	for ind := range appVerList.Items {
		existsAppVer = &appVerList.Items[ind]
		if existsAppVer.GetChartVersion() == chrt.Metadata.Version {
			klog.Infof("helm application version exists, id: %s", existsAppVer.Name)
			if existsAppVer.Spec.DataKey == "" && existsAppVer.Status.State == v1alpha1.StateActive {
				return existsAppVer, nil
			} else {
				continue
			}
		}
	}

	var appId string
	if existsAppVer == nil {
		appId = idutils.GetUuid36(v1alpha1.HelmApplicationVersionIdPrefix)
	} else {
		appId = existsAppVer.Name
	}

	// upload chart data
	if existsAppVer == nil || existsAppVer.Spec.DataKey == "" {
		err = wf.s3Cleint.Upload(path.Join(app.GetWorkspace(), appId), appId, chartFile)
		if err != nil {
			return nil, err
		}
	}

	// create new appVer
	if existsAppVer == nil {
		appVer := &v1alpha1.HelmApplicationVersion{
			ObjectMeta: metav1.ObjectMeta{
				Name: appId,
				Labels: map[string]string{
					constants.ChartApplicationIdLabelKey: app.Name,
				},
				Annotations: map[string]string{
					constants.CreatorAnnotationKey: "admin",
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						UID:        app.UID,
						Name:       app.Name,
						APIVersion: v1alpha1.SchemeGroupVersion.String(),
						Kind:       v1alpha1.ResourceKindHelmApplication,
					},
				},
			},
			Spec: v1alpha1.HelmApplicationVersionSpec{
				Metadata: &v1alpha1.Metadata{
					Name:       chrt.Name(),
					Version:    chrt.Metadata.Version,
					AppVersion: chrt.Metadata.AppVersion,
				},
				DataKey: appId,
			},
		}

		appVer, err = wf.client.HelmApplicationVersions().Create(ctx, appVer, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		existsAppVer = appVer
	}

	// update app version status
	return wf.UpdateAppVersionStatus(ctx, existsAppVer)
}

func (wf *ImportWorkFlow) UpdateAppVersionStatus(ctx context.Context, appVer *v1alpha1.HelmApplicationVersion) (*v1alpha1.HelmApplicationVersion, error) {
	if appVer.Status.State == v1alpha1.StateActive {
		return appVer, nil
	}

	retry := 5
	var err error
	for i := 0; i < retry; i++ {
		appVer.Status.State = v1alpha1.StateActive
		appVer.Status.Audit = append(appVer.Status.Audit, v1alpha1.Audit{
			State:    v1alpha1.StateActive,
			Time:     metav1.Now(),
			Operator: "admin",
		})

		name := appVer.Name
		appVer, err = wf.client.HelmApplicationVersions().UpdateStatus(ctx, appVer, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("update app version %s status failed, retry: %d, error: %s", name, i, err)
		} else {
			klog.Errorf("update app version %s status success", name)
			return appVer, nil
		}
		appVer, err = wf.client.HelmApplicationVersions().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("get helm application version %s failed, error: %s", name, err)
			return nil, err
		}
	}

	return appVer, nil
}
