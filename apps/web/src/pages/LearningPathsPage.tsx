import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { LearningPath } from "@kapp/client";
import {
  Badge,
  Button,
  Card,
  EmptyState,
  Field,
  Input,
  Modal,
  ModalClose,
  ModalContent,
  ModalDescription,
  ModalFooter,
  ModalHeader,
  ModalTitle,
  Select,
  Skeleton,
  Textarea,
  toast,
} from "@kapp/ui";
import {
  AlertTriangle,
  Clock,
  Compass,
  Plus,
  Route,
  Signal,
} from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeToken, statusVariant } from "../lib/ktypeView";
import { CoverArt, LmsPageHeader } from "../components/lms/primitives";

/**
 * LearningPathsPage lists a tenant's learning paths as consumer-grade
 * cards and offers a modal form to create one plus a one-click enroll
 * action per card. Reads/writes go through the FeatureLMS-gated
 * `/api/v1/lms/learning-paths` surface, so tenants without the flag get
 * a 403 that surfaces as an error state rather than a crash.
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
  const fmt = useFormatter();
  const [draft, setDraft] = useState<Draft>(emptyDraft());
  const [createOpen, setCreateOpen] = useState(false);
  const [submitted, setSubmitted] = useState(false);

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
    onSuccess: (created) => {
      const title = created?.title ?? draft.title.trim();
      setDraft(emptyDraft());
      setSubmitted(false);
      setCreateOpen(false);
      qc.invalidateQueries({ queryKey: ["lms", "learning-paths"] });
      toast.success("Learning path created", { description: title });
    },
    onError: (err) => {
      toast.error("Couldn't create learning path", {
        description: (err as Error).message,
      });
    },
  });

  const enroll = useMutation({
    mutationFn: (pathId: string) => api.enrollInLearningPath(pathId),
    onSuccess: (_data, pathId) => {
      const path = paths.find((p) => p.id === pathId);
      qc.invalidateQueries({ queryKey: ["lms", "learning-paths"] });
      toast.success("You're enrolled", { description: path?.title });
    },
    onError: (err) => {
      toast.error("Couldn't enroll", { description: (err as Error).message });
    },
  });

  const paths: LearningPath[] = pathsQ.data?.learning_paths ?? [];
  const titleError =
    submitted && !draft.title.trim() ? "Please enter a title." : undefined;

  function submitCreate() {
    setSubmitted(true);
    if (draft.title.trim()) createPath.mutate();
  }

  const newPathButton = (
    <Button leadingIcon={<Plus className="h-4 w-4" />} onClick={() => setCreateOpen(true)}>
      New path
    </Button>
  );

  return (
    <section className="flex flex-col gap-6">
      <LmsPageHeader
        area="Learning"
        title="Learning Paths"
        description="Curated sequences of courses. A path completes when all its mandatory courses are done."
        actions={newPathButton}
      />

      {pathsQ.isLoading ? (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {Array.from({ length: 3 }).map((_, i) => (
            <Card key={i} className="overflow-hidden">
              <Skeleton variant="rect" className="aspect-[16/9] w-full" />
              <div className="flex flex-col gap-3 p-4">
                <Skeleton variant="text" className="h-5 w-3/4" />
                <Skeleton variant="text" className="h-4 w-full" />
                <Skeleton variant="rect" className="h-9 w-full" />
              </div>
            </Card>
          ))}
        </div>
      ) : pathsQ.isError ? (
        <EmptyState
          icon={<AlertTriangle />}
          title="Couldn't load learning paths"
          description={(pathsQ.error as Error).message}
          action={
            <Button variant="secondary" onClick={() => pathsQ.refetch()}>
              Try again
            </Button>
          }
        />
      ) : paths.length === 0 ? (
        <EmptyState
          icon={<Compass />}
          title="No learning paths yet"
          description="Build your first path to guide learners through a sequence of courses."
          action={newPathButton}
        />
      ) : (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">
          {paths.map((p) => {
            const hours = p.estimated_duration_hours ?? 0;
            const roles = p.target_roles ?? [];
            return (
              <Card key={p.id} className="flex flex-col overflow-hidden">
                <CoverArt seed={p.title} icon={Route} />
                <div className="flex flex-1 flex-col gap-3 p-4">
                  <div className="flex items-start justify-between gap-2">
                    <h3 className="line-clamp-2 text-base font-semibold text-fg">
                      {p.title}
                    </h3>
                    <Badge variant={statusVariant(p.status)} className="shrink-0">
                      {humanizeToken(p.status)}
                    </Badge>
                  </div>

                  {p.description ? (
                    <p className="line-clamp-2 text-sm text-fg-muted">
                      {p.description}
                    </p>
                  ) : null}

                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="outline">
                      <Signal className="h-3 w-3" aria-hidden />
                      {humanizeToken(p.difficulty)}
                    </Badge>
                    {hours > 0 ? (
                      <span className="inline-flex items-center gap-1 text-xs text-fg-muted">
                        <Clock className="h-3.5 w-3.5" aria-hidden />
                        {fmt.number(hours)} hrs
                      </span>
                    ) : null}
                  </div>

                  {roles.length > 0 ? (
                    <div className="flex flex-wrap gap-1">
                      {roles.map((role) => (
                        <Badge key={role} variant="neutral" size="xs">
                          {humanizeToken(role)}
                        </Badge>
                      ))}
                    </div>
                  ) : null}

                  <div className="mt-auto pt-1">
                    <Button
                      variant="secondary"
                      size="sm"
                      className="w-full"
                      onClick={() => enroll.mutate(p.id)}
                      disabled={enroll.isPending && enroll.variables === p.id}
                    >
                      {enroll.isPending && enroll.variables === p.id
                        ? "Enrolling…"
                        : "Enroll"}
                    </Button>
                  </div>
                </div>
              </Card>
            );
          })}
        </div>
      )}

      <Modal
        open={createOpen}
        onOpenChange={(next) => {
          setCreateOpen(next);
          if (!next) setSubmitted(false);
        }}
      >
        <ModalContent>
          <ModalHeader>
            <ModalTitle>New learning path</ModalTitle>
            <ModalDescription>
              Curate a sequence of courses learners complete in order.
            </ModalDescription>
          </ModalHeader>
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => {
              e.preventDefault();
              submitCreate();
            }}
          >
            <Field
              label="Title"
              required
              error={titleError}
              help="The name learners will see for this path."
            >
              <Input
                value={draft.title}
                onChange={(e) => setDraft({ ...draft, title: e.target.value })}
                placeholder="e.g. New Manager Onboarding"
                autoFocus
              />
            </Field>

            <Field label="Description" help="Optional — what learners will gain.">
              <Textarea
                value={draft.description}
                onChange={(e) =>
                  setDraft({ ...draft, description: e.target.value })
                }
                placeholder="Summarize the goal of this path."
                rows={3}
              />
            </Field>

            <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <Field label="Difficulty">
                <Select
                  value={draft.difficulty}
                  onChange={(e) =>
                    setDraft({ ...draft, difficulty: e.target.value })
                  }
                >
                  {DIFFICULTIES.map((d) => (
                    <option key={d} value={d}>
                      {humanizeToken(d)}
                    </option>
                  ))}
                </Select>
              </Field>

              <Field label="Estimated hours" help="Optional.">
                <Input
                  type="number"
                  min="0"
                  value={draft.estimated_duration_hours}
                  onChange={(e) =>
                    setDraft({
                      ...draft,
                      estimated_duration_hours: e.target.value,
                    })
                  }
                  placeholder="0"
                />
              </Field>
            </div>

            <Field
              label="Target roles"
              help="Optional — comma-separated, e.g. Manager, Sales."
            >
              <Input
                value={draft.target_roles}
                onChange={(e) =>
                  setDraft({ ...draft, target_roles: e.target.value })
                }
                placeholder="Manager, Sales"
              />
            </Field>

            <ModalFooter>
              <ModalClose asChild>
                <Button type="button" variant="ghost">
                  Cancel
                </Button>
              </ModalClose>
              <Button type="submit" disabled={createPath.isPending}>
                {createPath.isPending ? "Creating…" : "Create path"}
              </Button>
            </ModalFooter>
          </form>
        </ModalContent>
      </Modal>
    </section>
  );
}

export default LearningPathsPage;
