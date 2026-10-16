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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

var _ = Describe("Certificate readiness", func() {
	// certificate builds a cert-manager Certificate whose status holds the
	// given conditions.
	certificate := func(conditions ...map[string]any) *unstructured.Unstructured {
		items := make([]any, 0, len(conditions))
		for _, c := range conditions {
			items = append(items, c)
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"status": map[string]any{"conditions": items},
		}}
	}
	// condition builds one entry of a Certificate's status.conditions.
	condition := func(condType, status, reason, message string) map[string]any {
		return map[string]any{
			"type": condType, "status": status, "reason": reason, "message": message,
		}
	}

	It("is True once cert-manager has issued the certificate", func() {
		got := certificateReadiness(certificate(condition(certManagerReadyType, "True", "Ready", "issued")), 3)

		Expect(got.Type).To(Equal(conditionCertificateReady))
		Expect(got.Status).To(Equal(metav1.ConditionTrue))
		Expect(got.Reason).To(Equal(reasonCertificateIssued))
		Expect(got.ObservedGeneration).To(Equal(int64(3)))
	})

	It("is False and keeps cert-manager's reason and message while it is issuing", func() {
		got := certificateReadiness(certificate(
			condition("Issuing", "True", "DoesNotExist", "Issuing certificate as Secret does not exist"),
			condition(certManagerReadyType, "False", "DoesNotExist", "Issuing certificate as Secret does not exist"),
		), 1)

		Expect(got.Status).To(Equal(metav1.ConditionFalse))
		Expect(got.Reason).To(Equal("DoesNotExist"))
		Expect(got.Message).To(Equal("Issuing certificate as Secret does not exist"))
	})

	It("is False and pending when the Certificate has no Ready condition yet", func() {
		got := certificateReadiness(certificate(condition("Issuing", "True", "Pending", "")), 1)

		Expect(got.Status).To(Equal(metav1.ConditionFalse))
		Expect(got.Reason).To(Equal(reasonCertificatePending))
	})

	It("is False and pending when the Certificate has no status at all", func() {
		got := certificateReadiness(&unstructured.Unstructured{Object: map[string]any{}}, 1)

		Expect(got.Status).To(Equal(metav1.ConditionFalse))
		Expect(got.Reason).To(Equal(reasonCertificatePending))
	})

	It("falls back to its own reason when cert-manager gives none", func() {
		got := certificateReadiness(certificate(condition(certManagerReadyType, "False", "", "")), 1)

		Expect(got.Status).To(Equal(metav1.ConditionFalse))
		Expect(got.Reason).To(Equal(reasonCertificatePending))
		Expect(got.Message).NotTo(BeEmpty())
	})
})
