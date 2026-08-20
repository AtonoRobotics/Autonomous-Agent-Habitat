# AMH Specification v8 - Validation & Review Summary

**Date:** 2026-08-20  
**Status:** ⚠️ INCOMPLETE - Blocking issues identified  
**Grade:** C+ (Coherent architecture, unfit for implementation)

---

## Critical Finding: Specification Truncation

**61% of v2 content removed in v8:**
- v2: 46,163 bytes (complete specification)
- v8: 17,973 bytes (truncated, placeholder text on line 101)

**Missing:**
- Sections §3–§13 (11 core subsystem specifications)
- Artifacts A–H (all critical schemas, interfaces, workflows, worked scenarios)
- Staged roadmap and completion thresholds
- End-to-end validation example

The specification ends with a placeholder:
> "[Specification continues with §3–§18, Artifacts A–H, staged roadmap, and comprehensive caveats — full v8 text from revision notes. For the complete section details, see the full document in the repository.]"

**This suggests v8 was published prematurely.**

---

## Architectural Assessment: Sound but Incomplete

### Strengths ✓

1. **Well-reasoned evolution (v2→v8)**
   - Each revision addresses a specific gap without contradicting prior decisions
   - v3: Native harness reimplementation (DeepAgents/DBOS collision)
   - v6: **Critical refinement** — reversibility ≠ physicality as sole gating axis
   - v8: SafetyCase framework for evidence-based autonomy on irreversible actions

2. **Clear organizing principle**
   - "Reversibility, not physicality, is the sole gating axis"
   - Unifies software capability rollback (§10) and physical device actuation (§12)

3. **Comprehensive decision table**
   - 18 subsystems, each with primary + runner-up choice
   - Well-justified trade-offs (Python agents + Go daemon balance addressed)

4. **Honest technology assessment**
   - Rejects over-architecture (Restate, Hatchet) for V1 single-node
   - Windows compatibility weighted appropriately
   - Polyglot approach justified, not ideological

### Blocking Issues ✗

1. **Revision notes reference non-existent sections**
   - "§7a (Spatial Memory)", "§14.6 (Reversible Capability Composition)", "§14.7 (Earned Autonomy)" are central to design but missing
   - Developers cannot verify architectural decisions

2. **No data schemas**
   - Artifact C (Ontology): No Location, Event, Action, Effect, Device, DeviceAction, SafetyCase entities defined
   - Artifact E (DDL): No sqlite-vec + FTS5 + graph schema with indices
   - Artifact D (Message Envelope): A2A-derived format claimed but not specified

3. **No interface contracts**
   - Artifact B (Protocols): No Python Protocols or TypeScript interfaces
   - SDK cannot be stable without formal interface definitions
   - `contracts/` directory is empty

4. **No implementation patterns**
   - Artifact F (Durable Workflows): DBOS Transact patterns for approve/execute/rollback not shown
   - §4 (Harness): Claims "native reimplementation of DeepAgents patterns" but doesn't specify which patterns
   - §3 (Context): "KV-cache-stable + FS offload + auto-compact" not algorithmically specified

5. **No validation/testing**
   - Artifact H (Worked Scenario): Greenhouse example removed from v8
   - No acceptance criteria, test vectors, or end-to-end validation path

---

## Implementation Readiness: Severely Limited

| Dimension | Score | Blocker |
|-----------|-------|---------|
| Spec completeness | 2/10 | 61% missing |
| Architectural coherence | 7/10 | Missing details undermine execution |
| Technology decisions | 6/10 | Compound decisions lack detail |
| Implementability | 2/10 | No schemas, interfaces, or workflows |
| Testability | 1/10 | Worked scenario absent |
| Extensibility | 3/10 | SDK contracts undefined |

**Cannot begin V0 implementation without:**
1. Ontology schema (Artifact C)
2. Data DDL (Artifact E)
3. Interface contracts (Artifact B)
4. DBOS workflow patterns (Artifact F)
5. Context compaction algorithm (§3 detail)
6. Harness implementation spec (§4 detail)

---

## Technology Claims Requiring Verification

### Tier 1 — Critical, Blocking (7 items)

- [ ] **DBOS Transact Python SDK SQLite support (PR #441, August 2025)** — Verify against GitHub
- [ ] **Temporal issue #3366 status** — "Support sqlite in production" claimed open with no milestone
- [ ] **Restate Windows binary (v1.7.4)** — Claimed unavailable as of spec date
- [ ] **Inngest Windows zips (v1.41.1)** — Claimed official release
- [ ] **KuzuDB archived October 2025** — Verify against GitHub timeline
- [ ] **A2A 1.0.1 (May 2026) release** — Verify against A2A repo (spec flags as "[reviewer-sourced]")
- [ ] **LadybugDB canonical Kuzu fork** — Verify against fork landscape (spec flags as "[reviewer-sourced]")

### Tier 2 — Important (3 items)

- [ ] Anthropic 90.2% multi-agent improvement claim — No methodology, no citation
- [ ] Manus & Cognition single-agent consensus — No publications referenced
- [ ] DeepAgents patterns MIT-licensed — Verify licensing terms

---

## Revision History Validation: Coherent

The v2→v8 progression is **logically sound**:

| Version | Gap Addressed | Resolution | Status |
|---------|-------------|-----------|--------|
| v3 | DeepAgents/DBOS durability collision | Native reimplementation on DBOS | ✓ Sound |
| v4 | Spatial deferral when robotics committed to geometry | Build spatial indexing into V1 core | ✓ Sound |
| v5 | Self-mod rollback under-specified | Add verified-inverse Effect tracking | ✓ Sound |
| v6 | Conflation of "physical" and "irreversible" | Reversibility as **sole** gating axis | ✓ Critical fix |
| v7 | Asymmetry in autonomy (software graduated, physical immediate) | Staged trust (`unverified`→`supervised`→`earned`) | ✓ Sound |
| v8 | Irreversible actions blocked from all earned autonomy | SafetyCase with evidence-based proof | ⚠️ Adds complexity, needs detail |

---

## Critical Gaps by Subsystem

| Section | Status | Why Missing | Severity |
|---------|--------|-------------|----------|
| §3 Context Engineering | ❌ MISSING | Budget thresholds, compaction triggers, VFS offload location not specified | 🔴 CRITICAL |
| §4 Deep-Agent Harness | ❌ MISSING | Which DeepAgents patterns? How integrated with DBOS? | 🔴 CRITICAL |
| §5–§9 | ❌ MISSING | Coder agents, ontology, memory, KB, inter-agent comms | 🔴 CRITICAL |
| §10–§13 | ❌ MISSING | Self-improvement, self-healing, integrations, observability | 🟠 HIGH |
| Artifact A (Monorepo) | ✓ Partial | README describes; not formally specified | 🟡 MEDIUM |
| Artifact B (Protocols) | ❌ MISSING | No Python Protocols or TypeScript interfaces | 🔴 CRITICAL |
| Artifact C (Ontology) | ❌ MISSING | No entity diagram or DDL | 🔴 CRITICAL |
| Artifact D (Envelope) | ❌ MISSING | A2A format claimed but not shown | 🟠 HIGH |
| Artifact E (DDL) | ❌ MISSING | Memory, KB, spatial schemas not provided | 🔴 CRITICAL |
| Artifact F (Workflows) | ❌ MISSING | DBOS step patterns missing | 🔴 CRITICAL |
| Artifact G (Manifests) | ❌ MISSING | Agent/Skill/Connector YAML specs not shown | 🟡 MEDIUM |
| Artifact H (Scenario) | ❌ MISSING | Greenhouse worked example removed from v8 | 🟠 HIGH |

---

## SafetyCase Framework (v8): Needs Clarification

**New concept introduced in v8 revision, but incomplete:**

**Undefined elements:**
1. Who/what is "agent-external independent review"? (Human? Audit system? Formal verifier?)
2. What constitutes "guardrails"? (Input validation? Outcome bounds?)
3. How is "supervised track record" quantified? (% success rate? Time threshold? Error patterns?)
4. What is "asymmetric single-incident revocation"? (Lose autonomy on one failure? How is it symmetric for reversible track?)
5. What is the decision tree? (risk_class + action_type → approval path?)

**Spec admits:** "The irreversible-action proof engine is explicitly not staged as a buildable V1/Post-V1 algorithm this spec does not invent."

→ **Operational ambiguity:** SafetyCase is promised but delegated to "real operational history and a defined independent-reviewer role this spec does not invent."

---

## Recommendations: Priority-Ordered Action Plan

### **Priority 1: Restore Missing Content (2–3 days)**

1. **Restore §3–§13 and Artifacts A–H from v2**
   - Source: Git history (available)
   - Approach: Selective restore, updating only v8 concepts (reversibility logic, SafetyCase framework)
   - Validate: Cross-references between sections, revision note integrations

2. **Formalize critical schemas**
   - Artifact C: Entity diagram + DDL (Location, Event, Action, Effect, Device, DeviceAction, SafetyCase)
   - Artifact E: sqlite-vec + FTS5 + graph DDL with indices, constraints, bi-temporal structure
   - Artifact D: JSON Schema for inter-agent envelope (A2A-derived format)

3. **Define interface contracts (Artifact B)**
   - Python Protocols: DurableEngine, Connector, Agent, Memory, KnowledgeBase, Context, MemoryStore, KnowledgeStore
   - TypeScript interfaces: Agent, Tool, Result, Message (SDK consumers)
   - Place in `contracts/` directory with version tags

4. **Specify durable workflows (Artifact F)**
   - DBOS Transact patterns: @workflow, @step, idempotency keys, circuit-breaker integration
   - Example flows: approve(DeviceAction), execute(DeviceAction), rollback(DeviceAction), verify(Effect)

### **Priority 2: Clarify v8 Concepts (1–2 days)**

5. **SafetyCase decision tree**
   - Specify: (risk_class, action_type) → approval_path (immediate | graduated | SafetyCase)
   - Define "agent-external review" role and escalation paths
   - Quantify "supervised track record" (success rates, error thresholds, time requirements)

6. **Context compaction algorithm (§3)**
   - Trigger: 70% context window budget
   - Mechanism: Summarize oldest N turns → JSON, retain last 3 turns verbatim, preserve cache-stable prefix
   - Storage: VFS location, durability model (sqlite? filesystem?)

7. **Earned autonomy graduation (§14.7 reversible track)**
   - Per-DeviceAction metrics: success rate, error frequency, recovery time
   - Threshold rules: When `unverified`→`supervised`? `supervised`→`earned`?
   - Link to GEPA pipeline: Proposal → evaluation → canary → promotion

### **Priority 3: Verification (1 week)**

8. **Fact-check technology claims**
   - Tier 1 blockers: DBOS PR #441, Temporal #3366, Restate Windows, KuzuDB timeline
   - Tier 2 important: Anthropic 90.2% claim, Manus/Cognition citations, DeepAgents licensing

9. **Specify model-routing extended reasoning (Row 14)**
   - Explain "reasoning_content round-trip handling"
   - Define adapter pattern for extended-reasoning models (DeepSeek, o1-class)
   - Token accounting for thinking vs. output tokens

10. **Observability instrumentation (§13)**
    - OTel spans per action type: DeviceAction, SelfModification, SubagentCall
    - Attributes: model, tokens_used, latency, outcome, rollback_status
    - Durable event replay format and retention policy

### **Priority 4: Documentation**

11. **Cross-reference map**
    - Show how v3–v8 revision notes update specific sections
    - Link architectural decisions to their justifications

12. **Rationale appendix**
    - Why not RDF/OWL? (over-build for single-node)
    - Why not Mem0? (standard taxonomy + bi-temporal > simpler)
    - Why not DSPy MIPROv2? (fewer rollouts, safer staging)

---

## Next Steps for Project

1. **Confirm v8 publication intent**: Was truncation unintentional?
2. **If unintentional**: Restore v2 content (2–3 day task), update for v8 concepts
3. **If intentional**: Provide rationale for removal and re-specify removed sections
4. **Fact-check blockers**: Begin Tier 1 verification (DBOS PR #441, Temporal #3366, etc.)
5. **Create CLAUDE.md**: Define AMH-specific validation rules, subsystem boundaries, and extension points
6. **Unblock V0**: Formal schema definitions allow parallel development (Go daemon scaffold, Python harness, connector SDK)

---

## Conclusion

**The AMH v8 specification is architecturally sound but operationally incomplete.** The design reasoning is clear through revision notes, but 61% of normative content is missing, making implementation impossible without guesswork. Immediate restoration of §3–§13 and Artifacts A–H is required, followed by clarification of v8's new concepts (SafetyCase, earned autonomy graduation).

**Actionable status:** 🔴 **Blocked on implementation; 2–3 weeks to full readiness with Priority 1–2 work.**

---

*Review completed: 2026-08-20*  
*Specification: docs/AMH-SPECIFICATION.md (v8, 2026-08-19)*
