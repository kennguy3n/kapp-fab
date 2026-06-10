import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { LearningPath } from "@kapp/client";
import {
  Badge,
  Button,
  Input,
  Select,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

/**
 * LearningPathsPage (Session 17, Deliverable 12).
 *
 * Lists a tenant's learning paths and offers a header form to create a
 * new one plus a one-click enroll action per row. All reads/writes go
 * through the FeatureLMS-gated `/api/v1/lms/learning-paths` surface, so
 * tenants without the flag get a 403 which surfaces here as an error
 * banner rather than a crash.
 */
const DIFFICULTIES = ["beginner", "intermediate", "advanced"];

type Draft = {
  title: string;
  description: string;
  difficulty: string;
  estimated_duration_hours: string;
  target_roles: string;
};

const emptyDraft = (): Draft => ({
  title: "",
  description: "",
  difficulty: "beginner",
  estimated_duration_hours: "",
  target_roles: "",
});

export function LearningPathsPage() {
  const qc = useQueryClient();
  const [draft, setDraft] = useState<Draft>(emptyDraft());

  const pathsQ = useQuery({
    queryKey: ["lms", "learning-paths"],
    queryFn: () => api.listLearningPaths(),
  });

  const createPath = useMutation({
    mutationFn: () =>
      api.createLearningPath({
        title: draft.title.trim(),
        description: draft.description.trim() || undefined,
        difficulty: draft.difficulty,
        estimated_duration_hours: draft.estimated_duration_hours
          ? Number(draft.estimated_duration_hours)
          : undefined,
        target_roles: draft.target_roles
          ? draft.target_roles
              .split(",")
              .map((r) => r.trim())
              .filter(Boolean)
          : undefined,
      }),
    onSuccess: () => {
      setDraft(emptyDraft());
      qc.invalidateQueries({ queryKey: ["lms", "learning-paths"] });
    },
  });

  const enroll = useMutation({
    mutationFn: (pathId: string) => api.enrollInLearningPath(pathId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["lms", "learning-paths"] });
    },
  });

  const paths: LearningPath[] = pathsQ.data?.learning_paths ?? [];

  return (
    <section>
      <h1>Learning Paths</h1>
      <p className="text-fg-muted">
        Curated sequences of courses. A path completes when all its
        mandatory courses are complete.
      </p>

      <form
        className="mt-4 flex flex-wrap items-end gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (draft.title.trim()) createPath.mutate();
        }}
      >
        <label className="flex flex-col text-[12px] text-fg-muted">
          Title
          <Input
            value={draft.title}
            onChange={(e) => setDraft({ ...draft, title: e.target.value })}
            placeholder="e.g. New Manager Onboarding"
            required
          />
        </label>
        <label className="flex flex-col text-[12px] text-fg-muted">
          Difficulty
          <Select
            value={draft.difficulty}
            onChange={(e) => setDraft({ ...draft, difficulty: e.target.value })}
          >
            {DIFFICULTIES.map((d) => (
              <option key={d} value={d}>
                {d}
              </option>
            ))}
          </Select>
        </label>
        <label className="flex flex-col text-[12px] text-fg-muted">
          Est. hours
          <Input
            type="number"
            min="0"
            value={draft.estimated_duration_hours}
            onChange={(e) =>
              setDraft({ ...draft, estimated_duration_hours: e.target.value })
            }
            className="w-24"
          />
        </label>
        <label className="flex flex-col text-[12px] text-fg-muted">
          Target roles (comma-separated)
          <Input
            value={draft.target_roles}
            onChange={(e) =>
              setDraft({ ...draft, target_roles: e.target.value })
            }
            placeholder="manager, sales"
          />
        </label>
        <Button type="submit" disabled={!draft.title.trim() || createPath.isPending}>
          {createPath.isPending ? "Creating…" : "+ New path"}
        </Button>
      </form>
      {createPath.isError && (
        <p className="mt-2 text-danger">
          Failed to create: {(createPath.error as Error).message}
        </p>
      )}

      {pathsQ.isLoading && <p className="mt-4">Loading…</p>}
      {pathsQ.isError && (
        <p className="mt-4 text-danger">
          Failed to load learning paths: {(pathsQ.error as Error).message}
        </p>
      )}
      {pathsQ.data && paths.length === 0 && (
        <p className="mt-4 text-fg-muted">No learning paths yet.</p>
      )}
      {paths.length > 0 && (
        <Table className="mt-4 text-[13px]">
          <TableHeader>
            <TableRow className="text-left">
              <TableHead>Title</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Difficulty</TableHead>
              <TableHead className="text-right">Est. hours</TableHead>
              <TableHead>Target roles</TableHead>
              <TableHead className="text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {paths.map((p) => (
              <TableRow key={p.id}>
                <TableCell>{p.title}</TableCell>
                <TableCell>
                  <Badge>{p.status}</Badge>
                </TableCell>
                <TableCell>{p.difficulty}</TableCell>
                <TableCell className="text-right">
                  {p.estimated_duration_hours}
                </TableCell>
                <TableCell>{(p.target_roles ?? []).join(", ")}</TableCell>
                <TableCell className="text-right">
                  <Button
                    variant="secondary"
                    onClick={() => enroll.mutate(p.id)}
                    disabled={enroll.isPending}
                  >
                    Enroll
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {enroll.isError && (
        <p className="mt-2 text-danger">
          Enroll failed: {(enroll.error as Error).message}
        </p>
      )}
    </section>
  );
}

export default LearningPathsPage;
