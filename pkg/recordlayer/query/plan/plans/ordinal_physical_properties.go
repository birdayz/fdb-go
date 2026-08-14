package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// OrdinalAddressingMode states whether one evaluation program retains local
// source-window roots or has been fully reanchored to its carrier.
type OrdinalAddressingMode uint8

const (
	OrdinalAddressingInvalid OrdinalAddressingMode = iota
	OrdinalAddressingSourceBound
	OrdinalAddressingCarrierBound
)

// OrdinalLayoutRequirement is a sealed physical input requirement.
type OrdinalLayoutRequirement interface {
	SatisfiedBy(values.OrdinalLayout) (bool, error)
	isOrdinalLayoutRequirementView()
}

type (
	sourceLayoutRequirement struct{ required values.RequiredBindings }
	exactLayoutRequirement  struct{ layout values.OrdinalLayout }
)

func (*sourceLayoutRequirement) isOrdinalLayoutRequirementView() {}
func (*exactLayoutRequirement) isOrdinalLayoutRequirementView()  {}

// RequireSources constructs a requirement from a values-owned binding-origin
// manifest. Only its local window sources participate in compatibility.
func RequireSources(required values.RequiredBindings) (OrdinalLayoutRequirement, error) {
	if err := values.ValidateRequiredBindingsAdmission(required); err != nil {
		return nil, err
	}
	return &sourceLayoutRequirement{required: required}, nil
}

// RequireExactLayout constructs a carrier-bound requirement.
func RequireExactLayout(layout values.OrdinalLayout) (OrdinalLayoutRequirement, error) {
	if err := values.ValidateOrdinalLayoutAdmission(layout); err != nil {
		return nil, err
	}
	return &exactLayoutRequirement{layout: layout}, nil
}

func (r *sourceLayoutRequirement) SatisfiedBy(layout values.OrdinalLayout) (bool, error) {
	if r == nil || r.required == nil {
		return false, &values.ResolutionError{ErrorCode: values.LayoutForeignValue, Path: "plan.requirement", Detail: "source requirement is malformed"}
	}
	return values.LayoutSatisfies(layout, r.required)
}

func (r *exactLayoutRequirement) SatisfiedBy(layout values.OrdinalLayout) (bool, error) {
	if r == nil || r.layout == nil || layout == nil {
		return false, &values.ResolutionError{ErrorCode: values.LayoutForeignValue, Path: "plan.requirement", Detail: "exact requirement or candidate is nil"}
	}
	return r.layout.RawEqual(layout), nil
}

// OrdinalLayoutRequirementsEqual reports whether two admitted requirements
// describe the same compatibility class. Exact requirements compare by the
// immutable layout value rather than wrapper identity, so independently built
// pass-through parents over the same physical layout converge when their
// requirements are accumulated in the memo. Source requirements compare by
// their immutable RequiredBindings identity: that is deliberately
// conservative because RequiredBindings does not expose its owner-current
// handle, and treating two independently collected manifests as equal without
// that handle would erase a potentially significant carrier distinction.
// Foreign, malformed and typed-nil views are never equal.
func OrdinalLayoutRequirementsEqual(left, right OrdinalLayoutRequirement) bool {
	switch exactLeft := left.(type) {
	case *sourceLayoutRequirement:
		exactRight, ok := right.(*sourceLayoutRequirement)
		return ok && exactLeft != nil && exactRight != nil &&
			exactLeft.required != nil && exactLeft.required == exactRight.required
	case *exactLayoutRequirement:
		exactRight, ok := right.(*exactLayoutRequirement)
		return ok && exactLeft != nil && exactRight != nil &&
			exactLeft.layout != nil && exactRight.layout != nil &&
			exactLeft.layout.RawEqual(exactRight.layout)
	default:
		return false
	}
}

// OrdinalEvaluationProgram pairs one evaluation layout with the exact origin
// manifest and addressing mode used by its retained Values.
type OrdinalEvaluationProgram interface {
	Layout() values.OrdinalLayout
	RequiredBindings() values.RequiredBindings
	AddressingMode() OrdinalAddressingMode
	isOrdinalEvaluationProgramView()
}

type ordinalEvaluationProgram struct {
	layout   values.OrdinalLayout
	required values.RequiredBindings
	mode     OrdinalAddressingMode
}

func (*ordinalEvaluationProgram) isOrdinalEvaluationProgramView() {}
func (p *ordinalEvaluationProgram) Layout() values.OrdinalLayout {
	if p == nil {
		return nil
	}
	return p.layout
}

func (p *ordinalEvaluationProgram) RequiredBindings() values.RequiredBindings {
	if p == nil {
		return nil
	}
	return p.required
}

func (p *ordinalEvaluationProgram) AddressingMode() OrdinalAddressingMode {
	if p == nil {
		return OrdinalAddressingInvalid
	}
	return p.mode
}

// NewOrdinalEvaluationProgram validates a complete physical evaluation phase.
func NewOrdinalEvaluationProgram(
	layout values.OrdinalLayout,
	required values.RequiredBindings,
	mode OrdinalAddressingMode,
) (OrdinalEvaluationProgram, error) {
	if layout == nil || required == nil {
		return nil, &values.ResolutionError{ErrorCode: values.LayoutForeignValue, Path: "plan.program", Detail: "layout and required bindings must be present"}
	}
	satisfied, err := values.LayoutSatisfies(layout, required)
	if err != nil {
		return nil, err
	}
	windows := required.WindowSources()
	switch mode {
	case OrdinalAddressingSourceBound:
		if len(windows) == 0 {
			return nil, fmt.Errorf("source-bound ordinal program has no local source window")
		}
		if !satisfied {
			return nil, &values.ResolutionError{ErrorCode: values.LayoutSourceNotProvided, Path: "plan.program", Detail: "layout does not satisfy source-bound program"}
		}
	case OrdinalAddressingCarrierBound:
		if len(windows) != 0 {
			return nil, fmt.Errorf("carrier-bound ordinal program retains local source windows")
		}
		if !satisfied {
			return nil, &values.ResolutionError{ErrorCode: values.LayoutSourceNotProvided, Path: "plan.program", Detail: "carrier-bound program and layout disagree"}
		}
	default:
		return nil, fmt.Errorf("invalid ordinal addressing mode %d", mode)
	}
	return &ordinalEvaluationProgram{layout: layout, required: required, mode: mode}, nil
}

// OrdinalPhysicalProperties is the immutable provided/required layout view of
// one physical plan. It is deliberately separate from logical identity.
type OrdinalPhysicalProperties interface {
	RequiredInputLayouts() []OrdinalLayoutRequirement
	EvaluationPrograms() []OrdinalEvaluationProgram
	ProvidedOutputLayout() values.OrdinalLayout
	isOrdinalPhysicalPropertiesView()
}

type ordinalPhysicalProperties struct {
	required []OrdinalLayoutRequirement
	programs []OrdinalEvaluationProgram
	provided values.OrdinalLayout
}

func (*ordinalPhysicalProperties) isOrdinalPhysicalPropertiesView() {}

func (p *ordinalPhysicalProperties) RequiredInputLayouts() []OrdinalLayoutRequirement {
	if p == nil {
		return nil
	}
	return append([]OrdinalLayoutRequirement(nil), p.required...)
}

func (p *ordinalPhysicalProperties) EvaluationPrograms() []OrdinalEvaluationProgram {
	if p == nil {
		return nil
	}
	return append([]OrdinalEvaluationProgram(nil), p.programs...)
}

func (p *ordinalPhysicalProperties) ProvidedOutputLayout() values.OrdinalLayout {
	if p == nil {
		return nil
	}
	return p.provided
}

// NewOrdinalPhysicalProperties constructs a complete physical-property view.
func NewOrdinalPhysicalProperties(
	required []OrdinalLayoutRequirement,
	programs []OrdinalEvaluationProgram,
	provided values.OrdinalLayout,
) (OrdinalPhysicalProperties, error) {
	if err := values.ValidateOrdinalLayoutAdmission(provided); err != nil {
		return nil, err
	}
	for i, requirement := range required {
		switch exact := requirement.(type) {
		case *sourceLayoutRequirement:
			if exact == nil || exact.required == nil {
				return nil, fmt.Errorf("plan input requirement %d is malformed", i)
			}
		case *exactLayoutRequirement:
			if exact == nil || exact.layout == nil {
				return nil, fmt.Errorf("plan input requirement %d is malformed", i)
			}
		default:
			return nil, fmt.Errorf("plan input requirement %d is foreign", i)
		}
	}
	for i, program := range programs {
		if exact, ok := program.(*ordinalEvaluationProgram); !ok || exact == nil {
			return nil, fmt.Errorf("plan evaluation program %d is foreign", i)
		}
	}
	return &ordinalPhysicalProperties{
		required: append([]OrdinalLayoutRequirement(nil), required...),
		programs: append([]OrdinalEvaluationProgram(nil), programs...),
		provided: provided,
	}, nil
}

// AsOrdinalPhysicalProperties exact-recognizes only plans-owned views.
func AsOrdinalPhysicalProperties(value any) (OrdinalPhysicalProperties, bool) {
	exact, ok := value.(*ordinalPhysicalProperties)
	if !ok || exact == nil || exact.provided == nil {
		return nil, false
	}
	return exact, true
}
