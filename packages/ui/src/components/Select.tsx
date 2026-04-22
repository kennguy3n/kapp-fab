import type { SelectHTMLAttributes, ReactNode } from "react";

interface SelectProps extends SelectHTMLAttributes<HTMLSelectElement> {
  children: ReactNode;
}

export function Select({ children, style, ...rest }: SelectProps) {
  return (
    <select
      {...rest}
      style={{
        padding: "6px 8px",
        borderRadius: 4,
        border: "1px solid #d1d5db",
        ...style,
      }}
    >
      {children}
    </select>
  );
}
