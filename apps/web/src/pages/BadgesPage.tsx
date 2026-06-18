import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import type { Badge as BadgeType, BadgeAward, KRecord } from "@kapp/client";
import {
  Avatar,
  AvatarFallback,
  Badge,
  Button,
  Card,
  EmptyState,
  Skeleton,
  cn,
  initials,
} from "@kapp/ui";
import { Award, AlertTriangle, Lock, Sparkles } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { humanizeLabel, humanizeToken } from "../lib/ktypeView";
import { LmsPageHeader } from "../components/lms/primitives";

/**
 * BadgesPage is the gamification surface: a catalog of badges shown as
 * earned-vs-locked medallions with their earn criteria, plus a feed of
 * recent awards. "Earned" reflects whether a badge has been awarded to
 * anyone in the tenant — the API doesn't expose a per-viewer award set,
 * so the catalog uses tenant-wide award counts. Data comes from the
 * FeatureLMS-gated gamification surface.
 */
function criteriaSummary(b: BadgeType): string {
  const entries = Object.entries(b.criteria_value ?? {});
  if (entries.length === 0) return humanizeToken(b.criteria_type);
  const parts = entries.map(([key, value]) => {
    const v =
      typeof value === "string"
        ? humanizeToken(value)
        : typeof value === "number" || typeof value === "boolean"
          ? String(value)
          : "";
    return v ? `${humanizeLabel(key)}: ${v}` : humanizeLabel(key);
  });
  return parts.join(" · ");
}

export function BadgesPage() {
  const fmt = useFormatter();

  const badgesQ = useQuery({
    queryKey: ["lms", "badges"],
    queryFn: () => api.listBadges(),
  });
  const awardsQ = useQuery({
    queryKey: ["lms", "badge-awards"],
    queryFn: () => api.listBadgeAwards(),
  });
  const employeesQ = useQuery({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
  });

  const badges: BadgeType[] = badgesQ.data?.badges ?? [];
  const awards: BadgeAward[] = awardsQ.data?.awards ?? [];

  const badgeNameById = useMemo(() => {
    const m = new Map<string, string>();
    badges.forEach((b) => m.set(b.id, b.name));
    return m;
  }, [badges]);

  const awardCountByBadge = useMemo(() => {
    const m = new Map<string, number>();
    awards.forEach((a) => m.set(a.badge_id, (m.get(a.badge_id) ?? 0) + 1));
    return m;
  }, [awards]);

  const learnerNameById = useMemo(() => {
    const m = new Map<string, string>();
    (employeesQ.data ?? []).forEach((e: KRecord) => {
      const d = e.data as Record<string, unknown>;
      if (typeof d.name === "string") m.set(e.id, d.name);
    });
    return m;
  }, [employeesQ.data]);

  const recentAwards = useMemo(
    () =>
      [...awards].sort(
        (x, y) =>
          new Date(y.earned_at).getTime() - new Date(x.earned_at).getTime(),
      ),
    [awards],
  );

  return (
    <section className="flex flex-col gap-8">
      <LmsPageHeader
        area="Achievements"
        title="Badges"
        description="Earn badges by completing courses and hitting milestones. Locked badges show how to unlock them."
      />

      <div className="flex flex-col gap-4">
        <h2 className="text-lg font-semibold text-fg">Badge catalog</h2>
        {badgesQ.isLoading ? (
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">
            {Array.from({ length: 3 }).map((_, i) => (
              <Skeleton key={i} variant="rect" className="h-36 w-full" />
            ))}
          </div>
        ) : badgesQ.isError ? (
          <EmptyState
            icon={<AlertTriangle />}
            title="Couldn't load badges"
            description={(badgesQ.error as Error).message}
            action={
              <Button variant="secondary" onClick={() => badgesQ.refetch()}>
                Try again
              </Button>
            }
          />
        ) : badges.length === 0 ? (
          <EmptyState
            icon={<Award />}
            title="No badges yet"
            description="Once badges are defined for this tenant, they'll appear here for learners to earn."
          />
        ) : (
          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">
            {badges.map((b) => {
              const count = awardCountByBadge.get(b.id) ?? 0;
              const earned = count > 0;
              return (
                <Card
                  key={b.id}
                  className={cn(
                    "flex flex-col gap-3 p-4",
                    !earned && "border-dashed",
                  )}
                >
                  <div className="flex items-start gap-3">
                    <div
                      className={cn(
                        "relative flex h-14 w-14 shrink-0 items-center justify-center rounded-full",
                        earned
                          ? "bg-gradient-to-br from-accent to-accent-hover text-accent-fg"
                          : "bg-bg-muted text-fg-subtle",
                      )}
                    >
                      <Award className="h-7 w-7" aria-hidden />
                      {!earned ? (
                        <span className="absolute -bottom-1 -right-1 rounded-full border border-border bg-bg-elevated p-0.5">
                          <Lock
                            className="h-3.5 w-3.5 text-fg-subtle"
                            aria-hidden
                          />
                        </span>
                      ) : null}
                    </div>
                    <div className="min-w-0 flex-1">
                      <div className="flex items-start justify-between gap-2">
                        <h3 className="truncate text-base font-semibold text-fg">
                          {b.name}
                        </h3>
                        {!b.active ? (
                          <Badge variant="warning" size="xs">
                            Inactive
                          </Badge>
                        ) : null}
                      </div>
                      {b.description ? (
                        <p className="mt-0.5 line-clamp-2 text-sm text-fg-muted">
                          {b.description}
                        </p>
                      ) : null}
                    </div>
                  </div>

                  <div className="mt-auto flex flex-col gap-2">
                    <p className="text-xs text-fg-subtle">
                      <span className="font-medium text-fg-muted">
                        How to earn:{" "}
                      </span>
                      {criteriaSummary(b)}
                    </p>
                    {earned ? (
                      <Badge variant="success" size="sm" className="self-start">
                        <Sparkles className="h-3 w-3" aria-hidden />
                        Earned by {fmt.number(count)}
                      </Badge>
                    ) : (
                      <Badge variant="neutral" size="sm" className="self-start">
                        <Lock className="h-3 w-3" aria-hidden />
                        Locked
                      </Badge>
                    )}
                  </div>
                </Card>
              );
            })}
          </div>
        )}
      </div>

      <div className="flex flex-col gap-4">
        <h2 className="text-lg font-semibold text-fg">Recent awards</h2>
        {awardsQ.isLoading ? (
          <ul className="flex flex-col gap-2">
            {Array.from({ length: 3 }).map((_, i) => (
              <li
                key={i}
                className="flex items-center gap-3 rounded-lg border border-border bg-bg-elevated p-3"
              >
                <Skeleton variant="circle" className="h-7 w-7" />
                <Skeleton variant="text" className="h-4 w-1/2" />
              </li>
            ))}
          </ul>
        ) : awardsQ.isError ? (
          <EmptyState
            icon={<AlertTriangle />}
            title="Couldn't load awards"
            description={(awardsQ.error as Error).message}
            action={
              <Button variant="secondary" onClick={() => awardsQ.refetch()}>
                Try again
              </Button>
            }
          />
        ) : recentAwards.length === 0 ? (
          <EmptyState
            icon={<Sparkles />}
            title="No awards yet"
            description="When learners earn badges, their achievements will show up here."
          />
        ) : (
          <ul className="flex flex-col gap-2">
            {recentAwards.map((a) => {
              const name = learnerNameById.get(a.user_id) || "Learner";
              const badgeName =
                badgeNameById.get(a.badge_id) ?? "a badge";
              return (
                <li
                  key={a.id}
                  className="flex items-center gap-3 rounded-lg border border-border bg-bg-elevated p-3"
                >
                  <Avatar size="sm">
                    <AvatarFallback>{initials(name)}</AvatarFallback>
                  </Avatar>
                  <div className="min-w-0 flex-1">
                    <p className="truncate text-sm text-fg">
                      <span className="font-medium">{name}</span> earned{" "}
                      <span className="font-medium">{badgeName}</span>
                    </p>
                    <p className="text-xs text-fg-subtle">
                      {fmt.date(new Date(a.earned_at))}
                    </p>
                  </div>
                  <Award className="h-5 w-5 shrink-0 text-accent" aria-hidden />
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </section>
  );
}

export default BadgesPage;
