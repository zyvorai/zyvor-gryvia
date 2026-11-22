// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: LicenseRef-Zyvor-Production-2.0

package budget

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func metav1Time(t time.Time) metav1.Time { return metav1.NewTime(t) }
