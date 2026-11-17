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
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformatorv1alpha1 "github.com/alexanderbakin/platformator/operator/api/v1alpha1"
)

const (
	// DefaultPostgresImage is the database image. It is the Alpine variant:
	// small, and it runs as a user the data directory can be created for.
	DefaultPostgresImage = "postgres:17-alpine"
	// DefaultBackupToolsImage uploads and downloads dumps.
	DefaultBackupToolsImage = "amazon/aws-cli:2.27.50"

	postgresPort     = 5432
	postgresPortName = "postgres"
	postgresUser     = "app"
	postgresDatabase = "app"

	// backupServiceAccount is the ServiceAccount of backup and restore pods,
	// created in each namespace that has a database. The cloud role trusts
	// this name and no other, so only these pods can reach the bucket.
	backupServiceAccount = "platformator-backup"

	// postgresLabel marks the pods of the database of an App, with the App's
	// name as value. postgresJobLabel does the same for backup and restore pods.
	postgresLabel    = "platformator.alexanderbakin.com/postgres"
	postgresJobLabel = "platformator.alexanderbakin.com/postgres-job"

	secretKeyDatabaseURL = "DATABASE_URL"
	secretKeyPassword    = "POSTGRES_PASSWORD"

	conditionPostgresReady = "PostgresReady"
	conditionBackupsReady  = "BackupsReady"

	reasonPostgresReady    = "PostgresReady"
	reasonPostgresStarting = "PostgresStarting"
	reasonRestoring        = "Restoring"
	reasonRestoreFailed    = "RestoreFailed"
	reasonBackupScheduled  = "BackupScheduled"
	reasonBackupNotSetUp   = "BackupNotConfigured"

	eventActionPostgres = "Postgres"

	// tokenAudience is what the cloud's token service expects in the token.
	tokenAudience = "sts.amazonaws.com"
	tokenDir      = "/var/run/secrets/cloud"
	// nobody is the user and group of backup pods: unprivileged, and the group
	// can read the projected token.
	nobody = int64(65534)

	// dropAll is the capability name that drops every Linux capability.
	dropAll corev1.Capability = "ALL"
)

func postgresName(app *platformatorv1alpha1.App) string { return app.Name + "-postgres" }

func postgresLabels(app *platformatorv1alpha1.App) map[string]string {
	return map[string]string{postgresLabel: app.Name}
}

func postgresJobLabels(app *platformatorv1alpha1.App) map[string]string {
	return map[string]string{postgresJobLabel: app.Name}
}

func (r *AppReconciler) postgresImage() string {
	if r.PostgresImage != "" {
		return r.PostgresImage
	}
	return DefaultPostgresImage
}

func (r *AppReconciler) backupToolsImage() string {
	if r.BackupToolsImage != "" {
		return r.BackupToolsImage
	}
	return DefaultBackupToolsImage
}

// backupsConfigured says whether the operator was given a bucket and a role.
func (r *AppReconciler) backupsConfigured() bool {
	return r.BackupBucket != "" && r.BackupRoleARN != ""
}

// postgresOutcome is what one pass learned about the database: the two
// conditions, both nil for an App without spec.postgres.
type postgresOutcome struct {
	ready   *metav1.Condition
	backups *metav1.Condition
}

// postgresEnv is the variable that points the App at its database. It comes
// first, so an entry of the same name in spec.env overrides it.
func postgresEnv(app *platformatorv1alpha1.App) []corev1.EnvVar {
	if app.Spec.Postgres == nil {
		return nil
	}
	return []corev1.EnvVar{{
		Name: secretKeyDatabaseURL,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: postgresName(app)},
			Key:                  secretKeyDatabaseURL,
		}},
	}}
}

// reconcilePostgres makes the database, its credentials and its backups. The
// credentials come first because the App's pods read them when they start.
func (r *AppReconciler) reconcilePostgres(
	ctx context.Context, app *platformatorv1alpha1.App,
) (postgresOutcome, error) {
	if app.Spec.Postgres == nil {
		return postgresOutcome{}, nil
	}
	if err := r.reconcilePostgresSecret(ctx, app); err != nil {
		return postgresOutcome{}, fmt.Errorf("credentials: %w", err)
	}
	if err := r.reconcilePostgresService(ctx, app); err != nil {
		return postgresOutcome{}, fmt.Errorf("service: %w", err)
	}
	if err := r.reconcilePostgresPolicies(ctx, app); err != nil {
		return postgresOutcome{}, fmt.Errorf("network policies: %w", err)
	}
	sts, err := r.reconcilePostgresStatefulSet(ctx, app)
	if err != nil {
		return postgresOutcome{}, fmt.Errorf("statefulset: %w", err)
	}

	ready := sts.Status.ReadyReplicas >= 1
	out := postgresOutcome{ready: &metav1.Condition{
		Type: conditionPostgresReady, ObservedGeneration: app.Generation,
		Status: metav1.ConditionFalse, Reason: reasonPostgresStarting,
		Message: "waiting for the database pod to be ready",
	}}
	if ready {
		out.ready.Status = metav1.ConditionTrue
		out.ready.Reason = reasonPostgresReady
		out.ready.Message = "the database accepts connections"
	}

	if app.Spec.Postgres.Backup != nil || app.Spec.Postgres.RestoreFrom != "" {
		if err := r.reconcileBackupServiceAccount(ctx, app); err != nil {
			return postgresOutcome{}, fmt.Errorf("backup service account: %w", err)
		}
	}
	if out.backups, err = r.reconcileBackups(ctx, app); err != nil {
		return postgresOutcome{}, fmt.Errorf("backups: %w", err)
	}
	if ready {
		if err = r.reconcileRestore(ctx, app, out.ready); err != nil {
			return postgresOutcome{}, fmt.Errorf("restore: %w", err)
		}
	}
	return out, nil
}

// reconcilePostgresSecret creates the Secret once. The password is never
// regenerated: the data directory keeps the one it was first started with.
func (r *AppReconciler) reconcilePostgresSecret(ctx context.Context, app *platformatorv1alpha1.App) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	key := client.ObjectKey{Namespace: app.Namespace, Name: postgresName(app)}
	var existing corev1.Secret
	err := reader.Get(ctx, key, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	raw := make([]byte, 24)
	if _, err = rand.Read(raw); err != nil {
		return err
	}
	password := hex.EncodeToString(raw)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: postgresLabels(app)},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			secretKeyPassword: password,
			// The connection never leaves the namespace and is limited by a
			// NetworkPolicy, so it is not encrypted; the password is hex, which
			// needs no escaping in a URL.
			secretKeyDatabaseURL: fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
				postgresUser, password, postgresName(app), postgresPort, postgresDatabase),
		},
	}
	if err = controllerutil.SetControllerReference(app, secret, r.Scheme); err != nil {
		return err
	}
	if err = r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *AppReconciler) reconcilePostgresService(ctx context.Context, app *platformatorv1alpha1.App) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: postgresName(app), Namespace: app.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.Selector = postgresLabels(app)
		svc.Spec.Ports = []corev1.ServicePort{{
			Name: postgresPortName, Port: postgresPort, TargetPort: intstr.FromInt32(postgresPort),
		}}
		return controllerutil.SetControllerReference(app, svc, r.Scheme)
	})
	return err
}

// reconcilePostgresPolicies isolates the database and its backup pods.
//
// The database accepts connections from the App's pods and from the backup
// and restore pods of the same App, and makes none itself. The backup pods
// may reach the database, DNS and the internet (the bucket), but never the
// node or the metadata service, as with build pods.
func (r *AppReconciler) reconcilePostgresPolicies(ctx context.Context, app *platformatorv1alpha1.App) error {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(postgresPort)
	pgPorts := []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}}

	db := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: postgresName(app), Namespace: app.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, db, func() error {
		db.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: postgresLabels(app)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					{PodSelector: &metav1.LabelSelector{MatchLabels: labelsFor(app)}},
					{PodSelector: &metav1.LabelSelector{MatchLabels: postgresJobLabels(app)}},
				},
				Ports: pgPorts,
			}},
			// No egress rules: the database starts no connections.
		}
		return controllerutil.SetControllerReference(app, db, r.Scheme)
	}); err != nil {
		return err
	}

	jobs := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: postgresName(app) + "-jobs", Namespace: app.Namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, jobs, func() error {
		jobs.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: postgresJobLabels(app)},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				dnsEgressRule(),
				internetEgressRule(),
				{
					To: []networkingv1.NetworkPolicyPeer{{
						PodSelector: &metav1.LabelSelector{MatchLabels: postgresLabels(app)},
					}},
					Ports: pgPorts,
				},
			},
		}
		return controllerutil.SetControllerReference(app, jobs, r.Scheme)
	})
	return err
}

func (r *AppReconciler) reconcilePostgresStatefulSet(
	ctx context.Context, app *platformatorv1alpha1.App,
) (*appsv1.StatefulSet, error) {
	size := app.Spec.Postgres.Size
	if size.IsZero() {
		size = resource.MustParse("1Gi")
	}
	secretRef := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: postgresName(app)}, Key: key,
		}}
	}
	noToken, noEscalation := false, false
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: postgresName(app), Namespace: app.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		replicas := int32(1)
		sts.Spec.Replicas = &replicas
		sts.Spec.ServiceName = postgresName(app)
		sts.Spec.Selector = &metav1.LabelSelector{MatchLabels: postgresLabels(app)}
		// The data goes with the App. A kept volume would hold a password that
		// nothing else remembers, since the Secret goes with the App too.
		sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		}
		if sts.CreationTimestamp.IsZero() {
			// The claim template cannot change after creation, so it is set once.
			// Growing the volume means editing the PVC, which the StatefulSet
			// controller does not do.
			sts.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: size},
					},
				},
			}}
		}
		sts.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: postgresLabels(app)},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: &noToken,
				SecurityContext: &corev1.PodSecurityContext{
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name:  postgresPortName,
					Image: r.postgresImage(),
					Ports: []corev1.ContainerPort{{Name: postgresPortName, ContainerPort: postgresPort}},
					Env: []corev1.EnvVar{
						{Name: "POSTGRES_USER", Value: postgresUser},
						{Name: "POSTGRES_DB", Value: postgresDatabase},
						{Name: "POSTGRES_PASSWORD", ValueFrom: secretRef(secretKeyPassword)},
						// A subdirectory, because the volume root holds lost+found on
						// some storage classes and initdb refuses a non-empty directory.
						{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/postgresql/data"}},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
							Command: []string{"pg_isready", "-U", postgresUser, "-d", postgresDatabase},
						}},
						PeriodSeconds: 5,
					},
					SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &noEscalation},
				}},
			},
		}
		return controllerutil.SetControllerReference(app, sts, r.Scheme)
	})
	return sts, err
}

// reconcileBackupServiceAccount creates the ServiceAccount of backup and
// restore pods. It belongs to the namespace, not to one App, so it has no
// owner and stays when an App goes.
func (r *AppReconciler) reconcileBackupServiceAccount(ctx context.Context, app *platformatorv1alpha1.App) error {
	noToken := false
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: backupServiceAccount, Namespace: app.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		// The token the cloud accepts is projected into the pod by hand, with
		// its own audience; the default token is not needed.
		sa.AutomountServiceAccountToken = &noToken
		return nil
	})
	return err
}

// backupPodSpec is the pod of a backup or restore Job: both talk to the
// database and to the bucket, with the cloud role and no stored keys.
func (r *AppReconciler) backupPodSpec(init, main corev1.Container) corev1.PodSpec {
	noToken, noEscalation, nonRoot := false, false, true
	uid := nobody
	locked := &corev1.SecurityContext{
		AllowPrivilegeEscalation: &noEscalation,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{dropAll}},
	}
	init.SecurityContext, main.SecurityContext = locked, locked
	expiry := int64(3600)
	for _, c := range []*corev1.Container{&init, &main} {
		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: "work", MountPath: "/work"},
			corev1.VolumeMount{Name: "cloud-token", MountPath: tokenDir, ReadOnly: true})
	}
	return corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		ServiceAccountName:           backupServiceAccount,
		AutomountServiceAccountToken: &noToken,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: &nonRoot, RunAsUser: &uid, FSGroup: &uid,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		InitContainers: []corev1.Container{init},
		Containers:     []corev1.Container{main},
		Volumes: []corev1.Volume{
			{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "cloud-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
					Audience: tokenAudience, ExpirationSeconds: &expiry, Path: "token",
				}}},
			}}},
		},
	}
}

// cloudEnv tells the AWS CLI to trade the projected token for the role.
func (r *AppReconciler) cloudEnv(app *platformatorv1alpha1.App) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "AWS_ROLE_ARN", Value: r.BackupRoleARN},
		{Name: "AWS_WEB_IDENTITY_TOKEN_FILE", Value: tokenDir + "/token"},
		{Name: "AWS_ROLE_SESSION_NAME", Value: app.Namespace + "-" + app.Name},
		{Name: "AWS_REGION", Value: r.BackupRegion},
		{Name: "AWS_EC2_METADATA_DISABLED", Value: "true"},
		{Name: "HOME", Value: "/work"},
		{Name: "BUCKET", Value: r.BackupBucket},
		{Name: "PREFIX", Value: app.Namespace + "/" + app.Name},
	}
}

func (r *AppReconciler) pgClientEnv(app *platformatorv1alpha1.App) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PGHOST", Value: postgresName(app)},
		{Name: "PGUSER", Value: postgresUser},
		{Name: "PGDATABASE", Value: postgresDatabase},
		{Name: "PGPASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: postgresName(app)}, Key: secretKeyPassword,
		}}},
	}
}

// reconcileBackups keeps the backup CronJob in line with spec.postgres.backup.
func (r *AppReconciler) reconcileBackups(
	ctx context.Context, app *platformatorv1alpha1.App,
) (*metav1.Condition, error) {
	backup := app.Spec.Postgres.Backup
	if backup == nil {
		return nil, nil
	}
	cond := &metav1.Condition{Type: conditionBackupsReady, ObservedGeneration: app.Generation}
	if !r.backupsConfigured() {
		cond.Status = metav1.ConditionFalse
		cond.Reason = reasonBackupNotSetUp
		cond.Message = "the platform has no backup bucket and role configured " +
			"(operator flags --backup-bucket and --backup-role-arn)"
		return cond, nil
	}

	schedule := backup.Schedule
	if schedule == "" {
		schedule = "0 3 * * *"
	}
	// The dump is made by the postgres image into a volume shared with the
	// next container, which uploads it. Each tool stays in its own image.
	dump := corev1.Container{
		Name: "dump", Image: r.postgresImage(), Env: r.pgClientEnv(app),
		Command: []string{"pg_dump", "--format=custom", "--file=/work/dump"},
	}
	upload := corev1.Container{
		Name: "upload", Image: r.backupToolsImage(), Env: r.cloudEnv(app),
		Command: []string{"/bin/sh", "-c"},
		Args: []string{
			`aws s3 cp /work/dump "s3://$BUCKET/$PREFIX/$(date -u +%Y%m%dT%H%M%SZ).dump" --only-show-errors`,
		},
	}
	cron := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: postgresName(app) + "-backup", Namespace: app.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cron, func() error {
		keep, retries, zone := int32(3), int32(2), "Etc/UTC"
		cron.Spec.Schedule = schedule
		cron.Spec.TimeZone = &zone
		cron.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
		cron.Spec.SuccessfulJobsHistoryLimit = &keep
		cron.Spec.FailedJobsHistoryLimit = &keep
		cron.Spec.JobTemplate.Spec.BackoffLimit = &retries
		cron.Spec.JobTemplate.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: postgresJobLabels(app)},
			Spec:       r.backupPodSpec(dump, upload),
		}
		return controllerutil.SetControllerReference(app, cron, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	cond.Status = metav1.ConditionTrue
	cond.Reason = reasonBackupScheduled
	cond.Message = fmt.Sprintf("a dump goes to s3://%s/%s/%s on the schedule %q",
		r.BackupBucket, app.Namespace, app.Name, schedule)
	return cond, nil
}

// restoreJobName names the Job for one restoreFrom value, so changing the
// value starts a new restore and the same value never starts a second one.
func restoreJobName(app *platformatorv1alpha1.App, file string) string {
	sum := sha256.Sum256([]byte(file))
	return postgresName(app) + "-restore-" + hex.EncodeToString(sum[:])[:8]
}

// reconcileRestore runs the restore for spec.postgres.restoreFrom once the
// database is ready, and records the result in status.postgres.restoredFrom.
// It changes ready when a restore is running or has failed.
func (r *AppReconciler) reconcileRestore(
	ctx context.Context, app *platformatorv1alpha1.App, ready *metav1.Condition,
) error {
	file := app.Spec.Postgres.RestoreFrom
	if file == "" {
		return nil
	}
	if app.Status.Postgres != nil && app.Status.Postgres.RestoredFrom == file {
		return nil
	}
	if !r.backupsConfigured() {
		ready.Status = metav1.ConditionFalse
		ready.Reason = reasonRestoreFailed
		ready.Message = "restoreFrom is set, but the platform has no backup bucket and role configured"
		return nil
	}

	name := restoreJobName(app, file)
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: name}, &job)
	if apierrors.IsNotFound(err) {
		download := corev1.Container{
			Name: "download", Image: r.backupToolsImage(), Env: r.cloudEnv(app),
			Command: []string{"/bin/sh", "-c"},
			Args:    []string{fmt.Sprintf(`aws s3 cp "s3://$BUCKET/$PREFIX/%s" /work/dump --only-show-errors`, file)},
		}
		restore := corev1.Container{
			Name: "restore", Image: r.postgresImage(), Env: r.pgClientEnv(app),
			Command: []string{
				"pg_restore", "--clean", "--if-exists", "--no-owner", "--dbname=" + postgresDatabase, "/work/dump",
			},
		}
		job = batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace}}
		backoff := int32(2)
		job.Spec.BackoffLimit = &backoff
		job.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: postgresJobLabels(app)},
			Spec:       r.backupPodSpec(download, restore),
		}
		if err = controllerutil.SetControllerReference(app, &job, r.Scheme); err != nil {
			return err
		}
		if err = r.Create(ctx, &job); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		err = nil
	}
	if err != nil {
		return err
	}

	switch {
	case job.Status.Succeeded > 0:
		app.Status.Postgres = &platformatorv1alpha1.PostgresStatus{RestoredFrom: file}
	case jobFailed(&job):
		ready.Status = metav1.ConditionFalse
		ready.Reason = reasonRestoreFailed
		ready.Message = fmt.Sprintf("restoring %s failed; see the logs of Job %s, and delete it to retry", file, name)
	default:
		ready.Status = metav1.ConditionFalse
		ready.Reason = reasonRestoring
		ready.Message = "restoring " + file
	}
	return nil
}

func jobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// applyPostgresConditions records the database conditions of a pass, and drops
// them, with the restore record, once the App has no database.
func applyPostgresConditions(app *platformatorv1alpha1.App, pg postgresOutcome) {
	switch {
	case pg.ready != nil:
		meta.SetStatusCondition(&app.Status.Conditions, *pg.ready)
	case app.Spec.Postgres == nil:
		meta.RemoveStatusCondition(&app.Status.Conditions, conditionPostgresReady)
		app.Status.Postgres = nil
	}
	switch {
	case pg.backups != nil:
		meta.SetStatusCondition(&app.Status.Conditions, *pg.backups)
	case app.Spec.Postgres == nil || app.Spec.Postgres.Backup == nil:
		meta.RemoveStatusCondition(&app.Status.Conditions, conditionBackupsReady)
	}
}

// emitPostgresEvent announces a change of PostgresReady or BackupsReady.
func (r *AppReconciler) emitPostgresEvent(
	app *platformatorv1alpha1.App, previous map[string]metav1.Condition, cond *metav1.Condition,
) {
	if r.Recorder == nil || cond == nil {
		return
	}
	if prev, found := previous[cond.Type]; found && prev.Status == cond.Status && prev.Reason == cond.Reason {
		return
	}
	eventType := corev1.EventTypeNormal
	if cond.Status == metav1.ConditionFalse && cond.Reason != reasonPostgresStarting && cond.Reason != reasonRestoring {
		eventType = corev1.EventTypeWarning
	}
	r.Recorder.Eventf(app, nil, eventType, cond.Reason, eventActionPostgres, "%s", cond.Message)
}
