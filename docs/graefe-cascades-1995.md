# The Cascades Framework for Query Optimization

**Author:** Goetz Graefe

**Source:** IEEE Data Engineering Bulletin, Vol. 18, No. 3, September 1995, pp. 19-28.

**Note:** This document is a detailed reference summary of the paper for use in this project's Cascades query optimizer implementation. For the full text, consult the original publication or the PDF in the project archive.

---

## Abstract (Summary)

The paper describes a new extensible query optimization framework that resolves shortcomings of the earlier EXODUS and Volcano optimizer generators. In addition to extensibility, dynamic programming, and memorization inherited from those prototypes, the Cascades optimizer provides:

1. Manipulation of operator arguments using rules or functions
2. Operators that are both logical and physical (e.g., predicates)
3. Schema-specific rules for materialized views
4. Rules to insert "enforcers" or "glue operators"
5. Rule-specific guidance, permitting grouping of rules
6. Basic facilities for parallel search, partially ordered cost measures, and dynamic plans
7. Extensive tracing support
8. A clean interface and implementation making full use of C++ abstraction mechanisms

The optimizer was operational and served as the foundation for query optimizers in Tandem's NonStop SQL and Microsoft's SQL Server.

---

## 1. Introduction

The paper traces the lineage from the EXODUS Optimizer Generator [GrD87] through the Volcano project [GrM93] to Cascades. Key contributions of each predecessor:

- **EXODUS:** Optimizer generator architecture based on declarative rules, logical and physical algebras, division into modular components, interface definitions for Database Implementor (DBI)-provided support functions.
- **Volcano:** Combined improved extensibility with an efficient search engine based on dynamic programming and memorization.

Flaws identified through applying the Volcano Optimizer Generator to an object-oriented database system [BMG93] and a scientific database system prototype [WoG93] motivated Cascades.

### Advantages of Cascades over Volcano (enumerated in the paper):

- Abstract interface classes defining the DBI-optimizer interface and permitting DBI-defined subclass hierarchies
- Rules as objects
- Facilities for schema- and even query-specific rules
- Simple rules requiring minimal DBI support
- Rules with substitutes consisting of a complex expression
- Rules that map an input pattern to a DBI-supplied function
- Rules to place property enforcers such as sort operations
- Operators that may be both logical and physical, e.g., predicates
- Patterns that match an entire subtree, e.g., a predicate
- Optimization tasks as data structures
- Incremental enumeration of equivalent logical expressions
- Guided or exhaustive search
- Ordering of moves by promise
- Rule-specific guidance
- Incremental improvement of estimated logical properties

---

## 2. Optimization Algorithm and Tasks

The optimization algorithm is broken into several parts called "tasks." Tasks are realized as objects with a "perform" method. Task objects are collected in a task structure (currently a LIFO stack, though other structures such as dependency graphs for parallel search are envisioned). Tasks can be reordered for heuristic guidance.

### The Six Task Types

The paper defines six task types whose relationships are shown in Figure 1:

```
Figure 1: Optimization Tasks

  +------------------+        +------------------+
  | Optimize Group   |<------>| Optimize Inputs  |
  +------------------+        +------------------+
          |                          ^
          v                          |
  +----------------------+    +-------------+
  | Optimize Expression  |--->| Apply Rule  |
  +----------------------+    +-------------+
          |                      ^    |
          v                      |    v
  +------------------+           |
  | Explore Group    |-----------+
  +------------------+
          ^
          |
  +----------------------+
  | Explore Expression   |
  +----------------------+

  Solid arrows = "schedules" (one task type invokes another)
  Dashed arrows = invocations pertaining to inputs (subqueries/subplans)
  Bidirectional arrows = mutual scheduling
```

**optimize()** copies the original query into the internal "memo" structure and triggers optimization of the root equivalence class.

### Optimize Group / Optimize Expression

- An **Optimize Group** task finds the best plan for any expression in the group. It applies rules to all expressions, combining each with a cost limit and required/excluded physical properties. It implements dynamic programming and memorization by checking whether the same optimization goal has been pursued already.
- **Optimizing an expression** starts with a single expression and applies rules to it. Optimize Group invokes Optimize Expression for each expression. Transitive rule applications find the best plan within the group.

### Explore Group / Explore Expression

- **Exploring** a group or expression is an entirely new concept with no equivalent in Volcano. It means deriving all logical expressions that match a given pattern, on demand.
- In Volcano, a first phase applied all transformation rules exhaustively to create all equivalent logical expressions; a second phase navigated that network applying implementation rules. Cascades abolishes this separation -- a group is explored using transformation rules only on demand, and only to create members matching a given pattern.
- Exploration tasks also avoid duplicate work via a "pattern memory" (initialized/administered by the DBI).

### Cascades vs. Volcano Search Strategy

- Volcano generates all equivalent logical expressions exhaustively in phase one; Cascades does this on demand.
- Without guidance, Cascades worst case equals Volcano's search strategy.
- With guidance, Cascades can avoid some exploration effort.
- Each expression in "memo" includes a bit map indicating which transformation rules have already been applied, preventing re-application.
- Risk: incorrect guidance can cause incorrect pruning. Two planned (but not yet implemented) techniques for guidance: (1) inspecting the rule set to determine reachability of operator mappings, and (2) DBI-implemented guidance mechanisms.

### Apply Rule Task

Performing "apply rule" has roughly four components:

1. **Binding derivation:** All bindings for the rule's pattern are derived and iterated. The binding procedure is recursive, realized as an iterator (the "BINDING" class with one instance per pattern node). Once a binding is found, it is translated into a tree of "EXPR" nodes (part of the DBI interface).
2. **Substitute creation:** For each binding, the rule's condition function is invoked and qualifying bindings are translated into the substitute. For function rules, the DBI provides a function that may be invoked repeatedly to create multiple substitutes (an iterator).
3. **Integration into memo:** Each substitute expression is integrated into the "memo" structure with search for and detection of duplicates (a recursive bottom-up process using hash tables for fast duplicate detection).
4. **Follow-on tasks:** If the substitute is a new expression, follow-on tasks are initiated depending on context (exploration vs. optimization) and whether the root operator is logical or physical.

### Optimize Inputs

The "optimize inputs" task is different from all other tasks: it schedules a follow-on task, waits for completion, resumes, and schedules the next. It optimizes input groups for a suitable optimization goal. After each input is optimized, it obtains the best execution cost and derives a new (tighter) cost limit for the next input, enabling aggressive pruning.

---

## 3. Data Abstraction and the User Interface

Design required three interleaved activities: (1) designing the DBI-optimizer interface (minimal, functional, clean abstractions), (2) implementing a prototype optimizer exercising the interface, (3) designing an efficient search strategy based on lessons from EXODUS and Volcano.

Interface design focused on: (i) clean abstractions for support functions, (ii) rule mechanisms permitting the DBI to manipulate operator arguments, (iii) concise and complete interface specifications.

Each interface class is designed to be a root of a subclass hierarchy. The DBI extends these classes freely; the optimizer relies only on the interface methods.

### 3.1 Operators and Their Arguments

- Operators are sets supported in the query language and the query evaluation engine -- logical and physical operators [Gra93].
- The "class OP-ARG" includes both logical and physical operators. Methods "is-logical" and "is-physical" classify each operator. An operator can be neither (useful for expansion grammars like Starburst [Loh88]), or both.
- Operator definitions include their arguments directly -- no separate "argument transfer" mechanism as in EXODUS. Two crucial facilities:
  - An operator can be both logical and physical (natural for single-record predicates, called "sargable" in System R [SAC79]).
  - Specific predicate transformations (e.g., splitting predicates for push-through-join) can be realized as rules invoking DBI-supplied functions (function rules), rather than as optimizer-interpreted rules.
- Two special operators for use in rules: **LEAF-OP** (matches any subtree; during matching, the extracted expression has leaf operators referring to equivalence classes) and **TREE-OP** (matches an entire expression of any depth/complexity, useful with function rules).
- Methods on operators:
  - "is-logical", "is-physical"
  - "opt-cutoff" -- determines how many moves to pursue (default: all, for exhaustive search)
  - Pattern matching and hashing methods (for logical operators)
  - Methods for finding and improving logical properties
  - Pattern memory initialization and exploration move decisions (for logical operators)
  - Physical output properties method (for physical operators)
  - Three cost methods: local cost, combined cost with inputs, and cost-limit verification between inputs
  - "input-reqd-prop" -- maps optimization goal to input optimization goal (cost limit + required/excluded physical properties)

### 3.2 Logical and Physical Properties, Costs

- **COST:** Very simple interface. Instances created/returned by operator methods. Only method (beyond construction/destruction/printing) is comparison.
- **SYNTH-LOG-PROP:** Encapsulation of logical properties. Only method is a hash function for faster duplicate expression retrieval. Does not apply to physical expressions.
- **SYNTH-PHYS-PROP:** No methods at all.
- **REQD-PHYS-PROP:** One method -- determines whether a synthesized physical property instance covers the required physical properties. If one set is more specific than another (e.g., sorted on "A, B, C" vs. required "A, B" only), comparison returns "MORE." Default returns "UNDEFINED."

### 3.3 Expression Trees

- The "class EXPR" communicates expressions (queries, plans, rules) between DBI and optimizer.
- Each instance is a tree node: an operator plus pointers to input nodes.
- Methods: extract operator, extract inputs, matching (recursively traverses two expression trees comparing operators).

### 3.4 Search Guidance

- The "class GUIDANCE" transfers optimization heuristics from one rule application to the next.
- Captures knowledge about the search process for future search activities.
- Simple guidance structures "ONCE-GUIDANCE" and "ONCE-RULE" ensure commutativity rules (and similar) are applied only once.
- Guidance structures facilitate dividing the rule set into invocable "modules" (cf. Mitchell et al. [MDZ93]).

### 3.5 Pattern Memory

- One instance of pattern memory per group, used to restrict exploration effort.
- Prevents redundant exploration of the same group for the same pattern.
- Before exploring a group for a pattern, the pattern memory is consulted; it decides whether exploration should proceed.
- The most complex pattern memory method is merging two pattern memories when two groups of equivalent expressions are discovered to be actually one.
- Interacts with the exploration promise function.

### 3.6 Rules

- Rules are objects (instances of "class RULE"), can be created at run-time.
- Unlike EXODUS/Volcano, Cascades does not divide rules into disjoint transformation/implementation sets. Instead, it invokes "is-logical" and "is-physical" on newly created expressions.
- A rule has: a name, an antecedent ("before" pattern), and a consequent (the substitute), both represented as expression trees.
- Both pattern and substitute can be arbitrarily complex. The EXODUS/Volcano restriction that implementation rule substitutes consist of only a single operator has been removed. The remaining restriction: all but the substitute's top operator must be logical.
- Two types of condition functions:
  - **Promise functions:** Invoked before exploration. Return a real value expressing how useful the rule might be (1.0 = normal, 0 or less = skip). Default: 0 if specific physical property required, 2 if implementation algorithm, 1 otherwise. Affects order/pruning of search, not correctness (if exhaustive search is chosen).
  - **Condition functions:** Invoked after exploration, before full binding extraction. Return Boolean -- is the rule truly applicable given the complete operator set?
- Additional rule methods: constructor, destructor, print, extract pattern/substitute/name/arity, "rule-type" (simple vs. function rule), "top-match" (built-in check before promise), "opt-cases" (how many physical property combinations to optimize for; default 1; exception: merge-join with two equality clauses needing two sort orders).
- Guidance creation methods: "opt-guidance," "expl-guidance," "input-opt-guidance," "input-expl-guidance" (all default to NULL).

### Rule Classification

- **Reduction rule:** Substitute is only a leaf operator (two groups in memo will be merged).
- **Expansion rule:** Pattern is only a leaf operator (always applicable). DBI must design appropriate promise/condition functions.
- **Enforcer rules:** Insert physical operators that enforce or guarantee desired physical properties (e.g., sort before merge-join). The rule's promise/condition must permit this only when the sort order is required, and the sort operator's "input-reqd-prop" must set excluded properties to avoid plans that already produce the desired order.

### Function Rules

- "Class FUNCTION-RULE": For situations where a function is easier to write than a rule set (e.g., dividing a complex join predicate into clauses).
- Once a matching expression is extracted, an iterator method is invoked repeatedly to create all substitutes.
- The extracted expression can be arbitrarily deep/complex if the TREE-OP is used in the pattern.
- Function rules and tree operators together permit the DBI to write virtually any transformation.

---

## 4. Future Work

Areas identified for future work:

1. Thorough evaluation and tuning of the optimizer
2. Building additional optimizers on the framework to expose weaknesses
3. Generators that produce Cascades specifications from higher-level data model and algebra descriptions
4. Improvements to the search strategy and implementation

Performance considerations: The strong separation between optimizer framework and DBI specification (extensive virtual methods, many references between structures, frequent object allocation/deallocation) has overhead. "De-modularization" could improve performance but would need measurement-based justification before sacrificing the clean separation.

---

## 5. Summary and Conclusions

Key advantages of Cascades over EXODUS and Volcano:

1. **Predicates as operators:** Predicates and other item operations can be modeled as part of the query and plan algebra. Operators that are both logical and physical can appear in both optimizer input (query) and output (plan). Function rules and TREE-OP permit direct manipulation of complex expression trees using DBI-supplied functions.
2. **Enforcers as normal operators:** Enforcers such as sorting are inserted into plans based on explicit rules (in Volcano, they were special operators not appearing in any rule).
3. **Guided and controlled search:** Both exploration (enumeration of equivalent logical expressions) and optimization (mapping logical to physical) can be guided and controlled by the DBI.
4. **Robust implementation:** Suitable for industrial deployment.

---

## 6. Acknowledgments

The query processing group at Tandem helped address hard problems unresolved in EXODUS and Volcano. David Maier was a sounding board for ideas during the design and development.

---

## 7. References

- **[BMG93]** J. A. Blakeley, W. J. McKenna, and G. Graefe, "Experiences Building the Open OODB Query Optimizer," Proc. ACM SIGMOD Conf., Washington, DC, May 1993, 287.
- **[GrD87]** G. Graefe and D. J. DeWitt, "The EXODUS Optimizer Generator," Proc. ACM SIGMOD Conf., San Francisco, CA, May 1987, 160.
- **[Gra93]** G. Graefe, "Query Evaluation Techniques for Large Databases," ACM Computing Surveys 25, 2 (June 1993), 73-170.
- **[GrM93]** G. Graefe and W. J. McKenna, "The Volcano Optimizer Generator: Extensibility and Efficient Search," Proc. IEEE Int'l. Conf. on Data Eng., Vienna, Austria, April 1993, 209.
- **[Loh88]** G. M. Lohman, "Grammar-Like Functional Rules for Representing Query Optimization Alternatives," Proc. ACM SIGMOD Conf., Chicago, IL, June 1988, 18.
- **[MDZ93]** G. Mitchell, U. Dayal, and S. B. Zdonik, "Control of an Extensible Query Optimizer: A Planning-Based Approach," Proc. Int'l. Conf. on Very Large Data Bases, Dublin, Ireland, August 1993, 517.
- **[SAC79]** P. G. Selinger, M. M. Astrahan, D. D. Chamberlin, R. A. Lorie, and T. G. Price, "Access Path Selection in a Relational Database Management System," Proc. ACM SIGMOD Conf., Boston, MA, May-June 1979, 23.
- **[WoG93]** R. H. Wolniewicz and G. Graefe, "Algebraic Optimization of Computations over Scientific Databases," Proc. Int'l Conf. on Very Large Data Bases, Dublin, Ireland, August 1993, 13.
