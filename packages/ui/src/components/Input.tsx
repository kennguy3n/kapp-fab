import type { InputHTMLAttributes } from "react";

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
  return (
    <input
      {...props}
      style={{
        padding: "6px 8px",
        borderRadius: 4,
        border: "1px solid #d1d5db",
        width: "100%",
        ...props.style,
      }}
    />
  );
}
