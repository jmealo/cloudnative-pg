---
name: Feature request
title: "[Feature]: Dynamic Storage Management (Meta-Synthesis)"
labels: ["triage", "enhancement", "storage"]
---

### Is there an existing issue already for this feature request/idea?
- [x] I have searched for an existing issue, and could not find anything. I believe this is a new feature request.

### What problem is this feature going to solve? Why should it be added?
Current storage management is fragmented. "Autoresize" handles growth but creates GitOps drift and replica size mismatches. "Shrink" is manual and high-toil. There is no unified controller that treats storage as a dynamic resource that breathes with the workload.

### Describe the solution you'd like
**Dynamic Storage Management**: An "Autopilot" for CNPG storage.

**API:**
- `request`: Provisioning Floor (Steady State).
- `limit`: Provisioning Ceiling (Emergency Cap).
- `targetBuffer`: Desired Free Space %.
- `maintenanceWindow`: Time slot for non-urgent convergence.

**Behavior:**
1. **Emergency Growth**: Immediate expansion when critical.
2. **Planned Convergence**: Grow/Shrink during maintenance windows to optimize cost.
3. **State Management**: Use `ClusterStatus` as the source of truth for volume size, ensuring new replicas always match the operational reality, not the stale spec.

### Describe alternatives you've considered
- **Variant 1 (Manual Shrink)**: Rejected due to GitOps drift issues.
- **Variant 2 (Lifecycle Policy)**: Absorbed into the `maintenanceWindow` logic.
- **Variant 3 (Operation CRD)**: Rejected to keep the model declarative.

### Additional context
This feature consolidates all previous storage RFCs into a single, definitive design.

### Backport?
No

### Are you willing to actively contribute to this feature?
Yes
