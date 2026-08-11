// Package delivery is the DELIVERY plane (docs/architecture.md).
//
// It contains the pluggable adapters — direct, flux, argocd — that consume
// identical rendered output and differ only in who calls apply. Adapter
// implementations land with M2 (ADR-0001).
package delivery
