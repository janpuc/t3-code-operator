package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func contentChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(update event.UpdateEvent) bool {
			if update.ObjectOld.GetGeneration() != update.ObjectNew.GetGeneration() {
				return true
			}
			oldTimestamp := update.ObjectOld.GetDeletionTimestamp()
			newTimestamp := update.ObjectNew.GetDeletionTimestamp()
			return (oldTimestamp == nil) != (newTimestamp == nil)
		},
	}
}
