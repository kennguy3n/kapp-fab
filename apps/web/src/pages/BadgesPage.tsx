import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import type { Badge as BadgeType, BadgeAward } from "@kapp/client";
import {
  Badge,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

/**
 * BadgesPage (Session 17, Deliverable 12).
 *
 * Two sections: the catalog of badges a tenant has defined and the
 * award history (who earned what, when). Both come from the
 * FeatureLMS-gated gamification surface.
 */
export function BadgesPage() {
  const badgesQ = useQuery({
    queryKey: ["lms", "badges"],
    queryFn: () => api.listBadges(),
  });
  const awardsQ = useQuery({
    queryKey: ["lms", "badge-awards"],
    queryFn: () => api.listBadgeAwards(),
  });

  const badges: BadgeType[] = badgesQ.data?.badges ?? [];
  const awards: BadgeAward[] = awardsQ.data?.awards ?? [];

  const badgeNameById = useMemo(() => {
    const m = new Map<string, string>();
    badges.forEach((b) => m.set(b.id, b.name));
    return m;
  }, [badges]);

  return (
    <section>
      <h1>Badges</h1>
      <p className="text-fg-muted">
        Gamification badges and the history of awards earned by learners.
      </p>

      <h2 className="mt-4">Catalog</h2>
      {badgesQ.isLoading && <p>Loading…</p>}
      {badgesQ.isError && (
        <p className="text-danger">
          Failed to load badges: {(badgesQ.error as Error).message}
        </p>
      )}
      {badgesQ.data && badges.length === 0 && (
        <p className="text-fg-muted">No badges defined yet.</p>
      )}
      {badges.length > 0 && (
        <Table className="mt-2 text-[13px]">
          <TableHeader>
            <TableRow className="text-left">
              <TableHead>Name</TableHead>
              <TableHead>Description</TableHead>
              <TableHead>Criteria</TableHead>
              <TableHead>Active</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {badges.map((b) => (
              <TableRow key={b.id}>
                <TableCell>{b.name}</TableCell>
                <TableCell>{b.description}</TableCell>
                <TableCell>
                  <Badge>{b.criteria_type}</Badge>
                </TableCell>
                <TableCell>{b.active ? "Yes" : "No"}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <h2 className="mt-6">Award history</h2>
      {awardsQ.isLoading && <p>Loading…</p>}
      {awardsQ.isError && (
        <p className="text-danger">
          Failed to load awards: {(awardsQ.error as Error).message}
        </p>
      )}
      {awardsQ.data && awards.length === 0 && (
        <p className="text-fg-muted">No badges awarded yet.</p>
      )}
      {awards.length > 0 && (
        <Table className="mt-2 text-[13px]">
          <TableHeader>
            <TableRow className="text-left">
              <TableHead>Badge</TableHead>
              <TableHead>Learner</TableHead>
              <TableHead>Earned</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {awards.map((a) => (
              <TableRow key={a.id}>
                <TableCell>
                  {badgeNameById.get(a.badge_id) ?? a.badge_id.slice(0, 8) + "…"}
                </TableCell>
                <TableCell>{a.user_id.slice(0, 8)}…</TableCell>
                <TableCell>
                  {new Date(a.earned_at).toLocaleDateString()}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </section>
  );
}

export default BadgesPage;
