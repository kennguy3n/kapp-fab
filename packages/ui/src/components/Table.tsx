import type { TableHTMLAttributes } from "react";

export function Table(props: TableHTMLAttributes<HTMLTableElement>) {
  return (
    <table
      {...props}
      style={{
        width: "100%",
        borderCollapse: "collapse",
        ...props.style,
      }}
    />
  );
}
