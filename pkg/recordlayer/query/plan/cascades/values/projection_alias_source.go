package values

import "fmt"

// ProjectionAliasSource is the structured source identity captured when the
// planner machinery mints a projection alias for an internal datum key.
//
// The source is deliberately independent of the Value that computes the slot.
// Physical planning can reanchor that Value onto an exact current-row carrier,
// but doing so must not rewrite the authored source that supplied the alias.
// Present distinguishes an uncaptured source from the zero correlation; callers
// must never recover a missing source by parsing the alias spelling.
type ProjectionAliasSource struct {
	Source  CorrelationIdentifier
	Present bool
}

// NewProjectionAliasSource constructs a present structured source. A zero
// correlation cannot name an authored source, so it stays absent.
func NewProjectionAliasSource(source CorrelationIdentifier) ProjectionAliasSource {
	if source.IsZero() {
		return ProjectionAliasSource{}
	}
	return ProjectionAliasSource{Source: source, Present: true}
}

// ValidateProjectionAliasSources checks a slot-parallel alias-source vector.
// A source can exist only for a machinery-minted alias, and absence must carry
// no hidden correlation. A short vector is valid and means uncaptured sources.
func ValidateProjectionAliasSources(
	sources []ProjectionAliasSource,
	aliasMinted []bool,
	slots int,
) error {
	if len(sources) > slots {
		return fmt.Errorf("projection alias sources have %d entries for %d slots", len(sources), slots)
	}
	for i, source := range sources {
		if !source.Present {
			if !source.Source.IsZero() {
				return fmt.Errorf("projection alias source %d is absent but carries correlation %s", i, source.Source)
			}
			continue
		}
		if source.Source.IsZero() {
			return fmt.Errorf("projection alias source %d is present with a zero correlation", i)
		}
		if i >= len(aliasMinted) || !aliasMinted[i] {
			return fmt.Errorf("projection alias source %d is present for a user-named slot", i)
		}
	}
	return nil
}
