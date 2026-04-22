import type { HTMLAttributes } from "react";

export function Card({ style, ...rest }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      {...rest}
      style={{
        border: "1px solid #e5e7eb",
        borderRadius: 8,
        padding: 16,
        background: "#fff",
        ...style,
      }}
    />
  );
}
