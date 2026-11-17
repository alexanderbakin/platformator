/*
Copyright 2026.

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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

const (
	testBucket  = "platformator-backups"
	testRoleARN = "arn:aws:iam::123456789012:role/platformator-backup"
	testDump    = "20261117T030000Z.dump"
)

var _ = Describe("App postgres", func() {
	ctx := context.Background()

	configured := func() *AppReconciler {
		return &AppReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain,
			BackupBucket: testBucket, BackupRoleARN: testRoleARN, BackupRegion: "eu-central-1",
		}
	}
	reconcileWith := func(r *AppReconciler, app *platformatorv1alpha1.App) {
		_, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace},
		})
		Expect(err).NotTo(HaveOccurred())
	}
	key := func(app *platformatorv1alpha1.App, suffix string) types.NamespacedName {
		return types.NamespacedName{Name: app.Name + suffix, Namespace: app.Namespace}
	}
	condition := func(app *platformatorv1alpha1.App, condType string) *metav1.Condition {
		fresh := &platformatorv1alpha1.App{}
		Expect(k8sClient.Get(ctx, key(app, ""), fresh)).To(Succeed())
		return meta.FindStatusCondition(fresh.Status.Conditions, condType)
	}
	create := func(name string, postgres *platformatorv1alpha1.Postgres) *platformatorv1alpha1.App {
		app := validApp(name)
		app.Spec.Postgres = postgres
		Expect(k8sClient.Create(ctx, app)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, app)).To(Succeed()) })
		return app
	}
	markReady := func(app *platformatorv1alpha1.App) {
		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(app, "-postgres"), sts)).To(Succeed())
		sts.Status.Replicas, sts.Status.ReadyReplicas = 1, 1
		Expect(k8sClient.Status().Update(ctx, sts)).To(Succeed())
	}

	It("runs a database and points the App at it", func() {
		app := create("pg-basic", &platformatorv1alpha1.Postgres{})
		reconcileWith(configured(), app)

		sts := &appsv1.StatefulSet{}
		Expect(k8sClient.Get(ctx, key(app, "-postgres"), sts)).To(Succeed())
		Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))
		Expect(sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted).
			To(Equal(appsv1.DeletePersistentVolumeClaimRetentionPolicyType))

		svc := &corev1.Service{}
		Expect(k8sClient.Get(ctx, key(app, "-postgres"), svc)).To(Succeed())
		Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(app, "-postgres"), secret)).To(Succeed())
		Expect(string(secret.Data[secretKeyDatabaseURL])).To(ContainSubstring("@" + app.Name + "-postgres:5432/app"))

		dep := &appsv1.Deployment{}
		Expect(k8sClient.Get(ctx, key(app, ""), dep)).To(Succeed())
		env := dep.Spec.Template.Spec.Containers[0].Env
		Expect(env).NotTo(BeEmpty())
		Expect(env[0].Name).To(Equal("DATABASE_URL"))
		Expect(env[0].ValueFrom.SecretKeyRef.Name).To(Equal(app.Name + "-postgres"))

		ready := condition(app, conditionPostgresReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(reasonPostgresStarting))

		markReady(app)
		reconcileWith(configured(), app)
		Expect(condition(app, conditionPostgresReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(app, conditionBackupsReady)).To(BeNil())
	})

	It("keeps the password it first generated", func() {
		app := create("pg-password", &platformatorv1alpha1.Postgres{})
		reconcileWith(configured(), app)
		first := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(app, "-postgres"), first)).To(Succeed())

		reconcileWith(configured(), app)
		second := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key(app, "-postgres"), second)).To(Succeed())
		Expect(second.Data[secretKeyPassword]).To(Equal(first.Data[secretKeyPassword]))
		Expect(first.Data[secretKeyPassword]).To(HaveLen(48))
	})

	It("lets only the App and its backup pods reach the database, and the database reach nothing", func() {
		app := create("pg-policy", &platformatorv1alpha1.Postgres{})
		reconcileWith(configured(), app)

		db := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, key(app, "-postgres"), db)).To(Succeed())
		Expect(db.Spec.PolicyTypes).To(ConsistOf(networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress))
		Expect(db.Spec.Egress).To(BeEmpty())
		Expect(db.Spec.Ingress).To(HaveLen(1))
		Expect(db.Spec.Ingress[0].From).To(ConsistOf(
			networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: labelsFor(app)}},
			networkingv1.NetworkPolicyPeer{PodSelector: &metav1.LabelSelector{MatchLabels: postgresJobLabels(app)}},
		))
		Expect(db.Spec.Ingress[0].Ports).To(HaveLen(1))
		Expect(db.Spec.Ingress[0].Ports[0].Port.IntValue()).To(Equal(5432))

		jobs := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, key(app, "-postgres-jobs"), jobs)).To(Succeed())
		Expect(jobs.Spec.Egress).To(HaveLen(3))
		Expect(jobs.Spec.Egress[1].To[0].IPBlock.Except).To(ContainElement(linkLocalCIDR))
		Expect(jobs.Spec.Ingress).To(BeEmpty())
	})

	Describe("backups", func() {
		It("says so when the platform has no bucket", func() {
			app := create("pg-nobucket", &platformatorv1alpha1.Postgres{Backup: &platformatorv1alpha1.PostgresBackup{}})
			reconcileWith(&AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain}, app)

			cond := condition(app, conditionBackupsReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(reasonBackupNotSetUp))
			err := k8sClient.Get(ctx, key(app, "-postgres-backup"), &batchv1.CronJob{})
			Expect(client.IgnoreNotFound(err)).To(Succeed())
			Expect(err).To(HaveOccurred())
		})

		It("schedules a dump that uploads with the cloud role and no stored keys", func() {
			app := create("pg-backup", &platformatorv1alpha1.Postgres{
				Backup: &platformatorv1alpha1.PostgresBackup{Schedule: "15 2 * * *"},
			})
			reconcileWith(configured(), app)

			cron := &batchv1.CronJob{}
			Expect(k8sClient.Get(ctx, key(app, "-postgres-backup"), cron)).To(Succeed())
			Expect(cron.Spec.Schedule).To(Equal("15 2 * * *"))
			Expect(cron.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent))

			pod := cron.Spec.JobTemplate.Spec.Template
			Expect(pod.Labels).To(HaveKeyWithValue(postgresJobLabel, app.Name))
			Expect(pod.Spec.ServiceAccountName).To(Equal(backupServiceAccount))
			Expect(*pod.Spec.SecurityContext.RunAsNonRoot).To(BeTrue())
			Expect(pod.Spec.InitContainers).To(HaveLen(1))
			Expect(pod.Spec.InitContainers[0].Args[0]).To(ContainSubstring("pg_isready"))
			Expect(pod.Spec.InitContainers[0].Args[0]).To(ContainSubstring("exec pg_dump --format=custom"))

			env := map[string]string{}
			for _, e := range pod.Spec.Containers[0].Env {
				env[e.Name] = e.Value
			}
			Expect(env).To(HaveKeyWithValue("AWS_ROLE_ARN", testRoleARN))
			Expect(env).To(HaveKeyWithValue("BUCKET", testBucket))
			Expect(env).To(HaveKeyWithValue("PREFIX", app.Namespace+"/"+app.Name))
			Expect(env).NotTo(HaveKey("AWS_ACCESS_KEY_ID"))
			Expect(env).NotTo(HaveKey("AWS_SECRET_ACCESS_KEY"))

			var projected *corev1.ServiceAccountTokenProjection
			for _, v := range pod.Spec.Volumes {
				if v.Projected != nil {
					projected = v.Projected.Sources[0].ServiceAccountToken
				}
			}
			Expect(projected).NotTo(BeNil())
			Expect(projected.Audience).To(Equal(tokenAudience))

			sa := &corev1.ServiceAccount{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: backupServiceAccount, Namespace: app.Namespace}, sa)).
				To(Succeed())

			cond := condition(app, conditionBackupsReady)
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Describe("restore", func() {
		It("waits for the database, then runs one Job for the dump", func() {
			app := create("pg-restore", &platformatorv1alpha1.Postgres{RestoreFrom: testDump})
			reconcileWith(configured(), app)
			Expect(k8sClient.Get(ctx, key(app, "-postgres-restore-"+restoreSuffix(app)), &batchv1.Job{})).NotTo(Succeed())

			markReady(app)
			reconcileWith(configured(), app)

			job := &batchv1.Job{}
			Expect(k8sClient.Get(ctx, key(app, "-postgres-restore-"+restoreSuffix(app)), job)).To(Succeed())
			Expect(job.Spec.Template.Spec.InitContainers[0].Args[0]).To(ContainSubstring("$PREFIX/" + testDump))
			Expect(job.Spec.Template.Spec.Containers[0].Args[0]).To(ContainSubstring("exec pg_restore --clean"))

			cond := condition(app, conditionPostgresReady)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(reasonRestoring))

			// A second pass finds the same Job instead of making another.
			reconcileWith(configured(), app)
			jobs := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobs, client.InNamespace(app.Namespace),
				client.MatchingLabels(postgresJobLabels(app)))).To(Succeed())
			Expect(jobs.Items).To(HaveLen(1))
		})

		It("does not restore a dump it has already restored", func() {
			app := create("pg-restored", &platformatorv1alpha1.Postgres{RestoreFrom: testDump})
			reconcileWith(configured(), app)
			markReady(app)

			fresh := &platformatorv1alpha1.App{}
			Expect(k8sClient.Get(ctx, key(app, ""), fresh)).To(Succeed())
			fresh.Status.Postgres = &platformatorv1alpha1.PostgresStatus{RestoredFrom: testDump}
			Expect(k8sClient.Status().Update(ctx, fresh)).To(Succeed())

			reconcileWith(configured(), app)
			err := k8sClient.Get(ctx, key(app, "-postgres-restore-"+restoreSuffix(app)), &batchv1.Job{})
			Expect(err).To(HaveOccurred())
			Expect(condition(app, conditionPostgresReady).Status).To(Equal(metav1.ConditionTrue))
		})

		It("refuses without a bucket", func() {
			app := create("pg-restore-nobucket", &platformatorv1alpha1.Postgres{RestoreFrom: testDump})
			bare := &AppReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Domain: testDomain}
			reconcileWith(bare, app)
			markReady(app)
			reconcileWith(bare, app)

			cond := condition(app, conditionPostgresReady)
			Expect(cond.Reason).To(Equal(reasonRestoreFailed))
		})
	})

	Describe("validation", func() {
		It("cannot reach another folder of the bucket through restoreFrom", func() {
			for _, bad := range []string{"../other/x.dump", "other/x.dump", "x.sql", ".dump", "/x.dump"} {
				app := validApp("pg-bad-restore")
				app.Spec.Postgres = &platformatorv1alpha1.Postgres{RestoreFrom: bad}
				Expect(k8sClient.Create(ctx, app)).NotTo(Succeed(), bad)
			}
		})

		It("cannot drop the database by removing the field", func() {
			app := create("pg-remove", &platformatorv1alpha1.Postgres{})
			fresh := &platformatorv1alpha1.App{}
			Expect(k8sClient.Get(ctx, key(app, ""), fresh)).To(Succeed())
			fresh.Spec.Postgres = nil
			err := k8sClient.Update(ctx, fresh)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("postgres cannot be removed"))
		})
	})
})

func restoreSuffix(app *platformatorv1alpha1.App) string {
	name := restoreJobName(app, testDump)
	return name[len(app.Name+"-postgres-restore-"):]
}
