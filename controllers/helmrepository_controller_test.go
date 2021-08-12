/*
Copyright 2020 The Flux authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/repo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/helmtestserver"
	"github.com/fluxcd/pkg/runtime/conditions"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	"github.com/fluxcd/source-controller/internal/helm"
)

var (
	testGetters = getter.Providers{
		getter.Provider{
			Schemes: []string{"http", "https"},
			New:     getter.NewHTTPGetter,
		},
	}
)

func TestHelmRepositoryReconciler_Reconcile(t *testing.T) {
	g := NewWithT(t)

	testServer, err := helmtestserver.NewTempHelmServer()
	g.Expect(err).NotTo(HaveOccurred())
	defer os.RemoveAll(testServer.Root())

	g.Expect(testServer.PackageChart("testdata/charts/helmchart")).To(Succeed())
	g.Expect(testServer.GenerateIndex()).To(Succeed())

	testServer.Start()
	defer testServer.Stop()

	obj := &sourcev1.HelmRepository{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "helmrepository-reconcile-",
			Namespace:    "default",
		},
		Spec: sourcev1.HelmRepositorySpec{
			Interval: metav1.Duration{Duration: interval},
			URL:      testServer.URL(),
		},
	}
	g.Expect(testEnv.Create(ctx, obj)).To(Succeed())

	key := client.ObjectKey{Name: obj.Name, Namespace: obj.Namespace}

	// Wait for finalizer to be set
	g.Eventually(func() bool {
		if err := testEnv.Get(ctx, key, obj); err != nil {
			return false
		}
		return len(obj.Finalizers) > 0
	}, timeout).Should(BeTrue())

	// Wait for HelmRepository to be Ready
	g.Eventually(func() bool {
		if err := testEnv.Get(ctx, key, obj); err != nil {
			return false
		}
		if !conditions.IsReady(obj) && obj.Status.Artifact == nil {
			return false
		}
		readyCondition := conditions.Get(obj, meta.ReadyCondition)
		return readyCondition.Status == metav1.ConditionTrue &&
			obj.Generation == readyCondition.ObservedGeneration
	}, timeout).Should(BeTrue())

	g.Expect(testEnv.Delete(ctx, obj)).To(Succeed())

	// Wait for HelmRepository to be deleted
	g.Eventually(func() bool {
		if err := testEnv.Get(ctx, key, obj); err != nil {
			return apierrors.IsNotFound(err)
		}
		return false
	}, timeout).Should(BeTrue())
}

func TestHelmRepositoryReconciler_reconcileStorage(t *testing.T) {
	tests := []struct {
		name             string
		beforeFunc       func(obj *sourcev1.HelmRepository, storage *Storage) error
		want             ctrl.Result
		wantErr          bool
		assertArtifact   *sourcev1.Artifact
		assertConditions []metav1.Condition
		assertPaths      []string
	}{
		{
			name: "garbage collects",
			beforeFunc: func(obj *sourcev1.HelmRepository, storage *Storage) error {
				revisions := []string{"a", "b", "c"}
				for n := range revisions {
					v := revisions[n]
					obj.Status.Artifact = &sourcev1.Artifact{
						Path:     fmt.Sprintf("/reconcile-storage/%s.txt", v),
						Revision: v,
					}
					if err := testStorage.MkdirAll(*obj.Status.Artifact); err != nil {
						return err
					}
					if err := testStorage.AtomicWriteFile(obj.Status.Artifact, strings.NewReader(v), 0644); err != nil {
						return err
					}
				}
				testStorage.SetArtifactURL(obj.Status.Artifact)
				return nil
			},
			assertArtifact: &sourcev1.Artifact{
				Path:     "/reconcile-storage/c.txt",
				Revision: "c",
				Checksum: "2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6",
				URL:      testStorage.Hostname + "/reconcile-storage/c.txt",
			},
			assertPaths: []string{
				"/reconcile-storage/c.txt",
				"!/reconcile-storage/b.txt",
				"!/reconcile-storage/a.txt",
			},
		},
		{
			name: "notices missing artifact in storage",
			beforeFunc: func(obj *sourcev1.HelmRepository, storage *Storage) error {
				obj.Status.Artifact = &sourcev1.Artifact{
					Path:     fmt.Sprintf("/reconcile-storage/invalid.txt"),
					Revision: "d",
				}
				testStorage.SetArtifactURL(obj.Status.Artifact)
				return nil
			},
			want: ctrl.Result{Requeue: true},
			assertPaths: []string{
				"!/reconcile-storage/invalid.txt",
			},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.ArtifactUnavailableCondition, "NoArtifact", "No artifact for resource in storage"),
			},
		},
		{
			name: "updates hostname on diff from current",
			beforeFunc: func(obj *sourcev1.HelmRepository, storage *Storage) error {
				obj.Status.Artifact = &sourcev1.Artifact{
					Path:     fmt.Sprintf("/reconcile-storage/hostname.txt"),
					Revision: "f",
					Checksum: "3b9c358f36f0a31b6ad3e14f309c7cf198ac9246e8316f9ce543d5b19ac02b80",
					URL:      "http://outdated.com/reconcile-storage/hostname.txt",
				}
				if err := testStorage.MkdirAll(*obj.Status.Artifact); err != nil {
					return err
				}
				if err := testStorage.AtomicWriteFile(obj.Status.Artifact, strings.NewReader("file"), 0644); err != nil {
					return err
				}
				return nil
			},
			assertPaths: []string{
				"/reconcile-storage/hostname.txt",
			},
			assertArtifact: &sourcev1.Artifact{
				Path:     "/reconcile-storage/hostname.txt",
				Revision: "f",
				Checksum: "3b9c358f36f0a31b6ad3e14f309c7cf198ac9246e8316f9ce543d5b19ac02b80",
				URL:      testStorage.Hostname + "/reconcile-storage/hostname.txt",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			r := &HelmRepositoryReconciler{
				Storage: testStorage,
			}

			obj := &sourcev1.HelmRepository{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-",
				},
			}
			if tt.beforeFunc != nil {
				g.Expect(tt.beforeFunc(obj, testStorage)).To(Succeed())
			}

			got, err := r.reconcileStorage(context.TODO(), obj)
			g.Expect(err != nil).To(Equal(tt.wantErr))
			g.Expect(got).To(Equal(tt.want))

			g.Expect(obj.Status.Artifact).To(MatchArtifact(tt.assertArtifact))
			if tt.assertArtifact != nil && tt.assertArtifact.URL != "" {
				g.Expect(obj.Status.Artifact.URL).To(Equal(tt.assertArtifact.URL))
			}
			g.Expect(obj.Status.Conditions).To(conditions.MatchConditions(tt.assertConditions))

			for _, p := range tt.assertPaths {
				absoluteP := filepath.Join(testStorage.BasePath, p)
				if !strings.HasPrefix(p, "!") {
					g.Expect(absoluteP).To(BeAnExistingFile())
					continue
				}
				g.Expect(absoluteP).NotTo(BeAnExistingFile())
			}
		})
	}
}

func TestHelmRepository_reconcileSource(t *testing.T) {
	type options struct {
		username   string
		password   string
		publicKey  []byte
		privateKey []byte
		ca         []byte
	}

	tests := []struct {
		name             string
		protocol         string
		server           options
		secret           *corev1.Secret
		beforeFunc       func(obj *sourcev1.HelmRepository)
		want             ctrl.Result
		wantErr          bool
		assertConditions []metav1.Condition
	}{
		{
			name:     "HTTP without secretRef makes ArtifactOutdated=True",
			protocol: "http",
			want:     ctrl.Result{RequeueAfter: interval},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.ArtifactOutdatedCondition, "NewRevision", "New index revision"),
			},
		},
		{
			name:     "HTTP with Basic Auth secret makes ArtifactOutdated=True",
			protocol: "http",
			server: options{
				username: "git",
				password: "1234",
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: "basic-auth",
				},
				Data: map[string][]byte{
					"username": []byte("git"),
					"password": []byte("1234"),
				},
			},
			beforeFunc: func(obj *sourcev1.HelmRepository) {
				obj.Spec.SecretRef = &meta.LocalObjectReference{Name: "basic-auth"}
			},
			want: ctrl.Result{RequeueAfter: interval},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.ArtifactOutdatedCondition, "NewRevision", "New index revision"),
			},
		},
		{
			name:     "HTTPS with CAFile secret makes ArtifactOutdated=True",
			protocol: "https",
			server: options{
				publicKey:  tlsPublicKey,
				privateKey: tlsPrivateKey,
				ca:         tlsCA,
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: "ca-file",
				},
				Data: map[string][]byte{
					"caFile": tlsCA,
				},
			},
			beforeFunc: func(obj *sourcev1.HelmRepository) {
				obj.Spec.SecretRef = &meta.LocalObjectReference{Name: "ca-file"}
			},
			want: ctrl.Result{RequeueAfter: interval},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.ArtifactOutdatedCondition, "NewRevision", "New index revision"),
			},
		},
		{
			name:     "HTTPS with invalid CAFile secret makes FetchFailed=True and returns error",
			protocol: "https",
			server: options{
				publicKey:  tlsPublicKey,
				privateKey: tlsPrivateKey,
				ca:         tlsCA,
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: "invalid-ca",
				},
				Data: map[string][]byte{
					"caFile": []byte("invalid"),
				},
			},
			beforeFunc: func(obj *sourcev1.HelmRepository) {
				obj.Spec.SecretRef = &meta.LocalObjectReference{Name: "invalid-ca"}
			},
			wantErr: true,
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.FetchFailedCondition, meta.FailedReason, "can't create TLS config for client: failed to append certificates from file"),
			},
		},
		{
			name:     "Invalid URL makes FetchFailed=True and returns zero Result",
			protocol: "http",
			beforeFunc: func(obj *sourcev1.HelmRepository) {
				obj.Spec.URL = strings.ReplaceAll(obj.Spec.URL, "http://", "")
			},
			want:    ctrl.Result{},
			wantErr: false,
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.FetchFailedCondition, sourcev1.URLInvalidReason, "first path segment in URL cannot contain colon"),
			},
		},
		{
			name:     "Unsupported scheme makes FetchFailed=True and returns zero Result",
			protocol: "http",
			beforeFunc: func(obj *sourcev1.HelmRepository) {
				obj.Spec.URL = strings.ReplaceAll(obj.Spec.URL, "http://", "ftp://")
			},
			want:    ctrl.Result{},
			wantErr: false,
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.FetchFailedCondition, meta.FailedReason, "scheme \"ftp\" not supported"),
			},
		},
		{
			name:     "Missing secret returns FetchFailed=True and returns error",
			protocol: "http",
			beforeFunc: func(obj *sourcev1.HelmRepository) {
				obj.Spec.SecretRef = &meta.LocalObjectReference{Name: "non-existing"}
			},
			wantErr: true,
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.FetchFailedCondition, sourcev1.AuthenticationFailedReason, "secrets \"non-existing\" not found"),
			},
		},
		{
			name:     "Malformed secret returns FetchFailed=True and returns error",
			protocol: "http",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: "malformed-basic-auth",
				},
				Data: map[string][]byte{
					"username": []byte("git"),
				},
			},
			beforeFunc: func(obj *sourcev1.HelmRepository) {
				obj.Spec.SecretRef = &meta.LocalObjectReference{Name: "malformed-basic-auth"}
			},
			wantErr: true,
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(sourcev1.FetchFailedCondition, sourcev1.AuthenticationFailedReason, "required fields 'username' and 'password"),
			},
		},
	}

	for _, tt := range tests {
		obj := &sourcev1.HelmRepository{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "auth-strategy-",
			},
			Spec: sourcev1.HelmRepositorySpec{
				Interval: metav1.Duration{Duration: interval},
				Timeout:  &metav1.Duration{Duration: interval},
			},
		}

		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			server, err := helmtestserver.NewTempHelmServer()
			g.Expect(err).NotTo(HaveOccurred())
			defer os.RemoveAll(server.Root())

			g.Expect(server.PackageChart("testdata/charts/helmchart")).To(Succeed())
			g.Expect(server.GenerateIndex()).To(Succeed())

			if len(tt.server.username+tt.server.password) > 0 {
				server.WithMiddleware(func(handler http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						u, p, ok := r.BasicAuth()
						if !ok || u != tt.server.username || p != tt.server.password {
							w.WriteHeader(401)
							return
						}
						handler.ServeHTTP(w, r)
					})
				})
			}

			secret := tt.secret.DeepCopy()
			switch tt.protocol {
			case "http":
				server.Start()
				defer server.Stop()
				obj.Spec.URL = server.URL()
			case "https":
				g.Expect(server.StartTLS(tt.server.publicKey, tt.server.privateKey, tt.server.ca, "example.com")).To(Succeed())
				defer server.Stop()
				obj.Spec.URL = server.URL()
			default:
				t.Fatalf("unsupported protocol %q", tt.protocol)
			}

			if tt.beforeFunc != nil {
				tt.beforeFunc(obj)
			}

			builder := fakeclient.NewClientBuilder().WithScheme(testEnv.GetScheme())
			if secret != nil {
				builder.WithObjects(secret.DeepCopy())
			}

			r := &HelmRepositoryReconciler{
				Client:  builder.Build(),
				Storage: testStorage,
				Getters: testGetters,
			}

			var artifact sourcev1.Artifact
			var index helm.ChartRepository
			got, err := r.reconcileSource(logr.NewContext(ctx, log.NullLogger{}), obj, &artifact, &index)
			g.Expect(obj.Status.Conditions).To(conditions.MatchConditions(tt.assertConditions))
			g.Expect(err != nil).To(Equal(tt.wantErr))
			g.Expect(got).To(Equal(tt.want))
			g.Expect(artifact).ToNot(BeNil())
		})
	}
}

func TestHelmRepositoryReconciler_reconcileArtifact(t *testing.T) {
	tests := []struct {
		name             string
		beforeFunc       func(t *WithT, obj *sourcev1.HelmRepository, artifact sourcev1.Artifact, index *helm.ChartRepository)
		afterFunc        func(t *WithT, obj *sourcev1.HelmRepository)
		want             ctrl.Result
		wantErr          bool
		assertConditions []metav1.Condition
	}{
		{
			name: "Archiving artifact to storage makes Ready=True",
			beforeFunc: func(t *WithT, obj *sourcev1.HelmRepository, artifact sourcev1.Artifact, index *helm.ChartRepository) {
				obj.Spec.Interval = metav1.Duration{Duration: interval}
			},
			want: ctrl.Result{RequeueAfter: interval},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(meta.ReadyCondition, meta.SucceededReason, "Stored artifact for revision 'existing'"),
			},
		},
		{
			name: "Up-to-date artifact should not update status",
			beforeFunc: func(t *WithT, obj *sourcev1.HelmRepository, artifact sourcev1.Artifact, index *helm.ChartRepository) {
				obj.Spec.Interval = metav1.Duration{Duration: interval}
				obj.Status.Artifact = artifact.DeepCopy()
			},
			afterFunc: func(t *WithT, obj *sourcev1.HelmRepository) {
				t.Expect(obj.Status.URL).To(BeEmpty())
			},
			want: ctrl.Result{RequeueAfter: interval},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(meta.ReadyCondition, meta.SucceededReason, "Stored artifact for revision 'existing'"),
			},
		},
		{
			name: "Removes ArtifactUnavailableCondition after creating artifact",
			beforeFunc: func(t *WithT, obj *sourcev1.HelmRepository, artifact sourcev1.Artifact, index *helm.ChartRepository) {
				obj.Spec.Interval = metav1.Duration{Duration: interval}
				conditions.MarkTrue(obj, sourcev1.ArtifactUnavailableCondition, "Foo", "")
			},
			want: ctrl.Result{RequeueAfter: interval},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(meta.ReadyCondition, meta.SucceededReason, "Stored artifact for revision 'existing'"),
			},
		},
		{
			name: "Removes ArtifactOutdatedCondition after creating a new artifact",
			beforeFunc: func(t *WithT, obj *sourcev1.HelmRepository, artifact sourcev1.Artifact, index *helm.ChartRepository) {
				obj.Spec.Interval = metav1.Duration{Duration: interval}
				conditions.MarkTrue(obj, sourcev1.ArtifactOutdatedCondition, "Foo", "")
			},
			want: ctrl.Result{RequeueAfter: interval},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(meta.ReadyCondition, meta.SucceededReason, "Stored artifact for revision 'existing'"),
			},
		},
		{
			name: "Creates latest symlink to the created artifact",
			beforeFunc: func(t *WithT, obj *sourcev1.HelmRepository, artifact sourcev1.Artifact, index *helm.ChartRepository) {
				obj.Spec.Interval = metav1.Duration{Duration: interval}
			},
			afterFunc: func(t *WithT, obj *sourcev1.HelmRepository) {
				localPath := testStorage.LocalPath(*obj.GetArtifact())
				symlinkPath := filepath.Join(filepath.Dir(localPath), "latest.tar.gz")
				targetFile, err := os.Readlink(symlinkPath)
				t.Expect(err).NotTo(HaveOccurred())
				t.Expect(localPath).To(Equal(targetFile))
			},
			want: ctrl.Result{RequeueAfter: interval},
			assertConditions: []metav1.Condition{
				*conditions.TrueCondition(meta.ReadyCondition, meta.SucceededReason, "Stored artifact for revision 'existing'"),
			},
		},
		{
			name: "Index is empty",
			beforeFunc: func(t *WithT, obj *sourcev1.HelmRepository, artifact sourcev1.Artifact, index *helm.ChartRepository) {
				*index = helm.ChartRepository{}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			r := &HelmRepositoryReconciler{
				Storage: testStorage,
			}

			obj := &sourcev1.HelmRepository{
				TypeMeta: metav1.TypeMeta{
					Kind: sourcev1.HelmRepositoryKind,
				},
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "test-bucket-",
					Generation:   1,
					Namespace:    "default",
				},
				Spec: sourcev1.HelmRepositorySpec{
					Timeout: &metav1.Duration{Duration: timeout},
				},
			}

			dirI, err := repo.IndexDirectory("testdata/charts", "https://example.com")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(dirI).ToNot(BeNil())

			dirIB, err := yaml.Marshal(dirI)
			g.Expect(err).NotTo(HaveOccurred())
			i, err := helm.NewChartRepository("https://example.com", testGetters, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(i).ToNot(BeNil())
			g.Expect(i.LoadIndex(dirIB)).To(Succeed())

			artifact := testStorage.NewArtifactFor(obj.Kind, obj, "existing", "foo.tar.gz")
			artifact.Checksum = i.Checksum

			if tt.beforeFunc != nil {
				tt.beforeFunc(g, obj, artifact, i)
			}

			got, err := r.reconcileArtifact(logr.NewContext(ctx, log.NullLogger{}), obj, artifact, *i)
			g.Expect(err != nil).To(Equal(tt.wantErr))
			g.Expect(got).To(Equal(tt.want))

			// On error, artifact is empty. Check artifacts only on successful
			// reconcile.
			if !tt.wantErr {
				g.Expect(obj.Status.Artifact).To(MatchArtifact(artifact.DeepCopy()))
			}
			g.Expect(obj.Status.Conditions).To(conditions.MatchConditions(tt.assertConditions))

			if tt.afterFunc != nil {
				tt.afterFunc(g, obj)
			}
		})
	}
}
