import { Badge, type BadgeProps } from "@kapp/ui";
import { titleCase } from "./format";

// Maps the sales/procurement workflow statuses to the semantic Badge
// colour scale so a document's state reads by colour before the label
// is parsed. Unknown statuses fall back to a neutral outline badge.
const STATUS_VARIANT: Record<string, NonNullable<BadgeProps["variant"]>> = {
  draft: "neutral",
  requested: "neutral",
  confirmed: "info",
  approved: "info",
  fulfilled: "success",
  received: "success",
  refunded: "success",
  ordered: "success",
  cancelled: "danger",
};

export interface StatusBadgeProps {
  status: string;
  size?: BadgeProps["size"];
}

export function StatusBadge({ status, size }: StatusBadgeProps) {
  const variant = STATUS_VARIANT[status] ?? "outline";
  return (
    <Badge variant={variant} size={size}>
      {titleCase(status)}
    </Badge>
  );
}
