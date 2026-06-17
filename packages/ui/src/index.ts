// @kapp/ui public surface — every component the host apps
// (apps/web, apps/storybook, future micro-frontends) consume
// is re-exported from this barrel.  Importing from anywhere
// other than "@kapp/ui" is unsupported — internal paths may
// move between minor versions, but this barrel is the stable
// contract.

export { cn } from "./lib/cn";

export { Button, buttonVariants, type ButtonProps } from "./components/Button";
export { Input, inputVariants, type InputProps } from "./components/Input";
export {
  Select,
  selectVariants,
  type SelectProps,
} from "./components/Select";
export {
  Textarea,
  textareaVariants,
  type TextareaProps,
} from "./components/Textarea";
export { Field, type FieldProps } from "./components/Field";
export { Eyebrow, type EyebrowProps } from "./components/Eyebrow";
export {
  Card,
  CardHeader,
  CardTitle,
  CardDescription,
  CardContent,
  CardFooter,
} from "./components/Card";
export {
  Table,
  TableHeader,
  TableBody,
  TableFooter,
  TableRow,
  TableHead,
  TableCell,
  TableCaption,
} from "./components/Table";
export {
  Modal,
  ModalTrigger,
  ModalPortal,
  ModalClose,
  ModalOverlay,
  ModalContent,
  ModalHeader,
  ModalFooter,
  ModalTitle,
  ModalDescription,
  ControlledModal,
  type ControlledModalProps,
} from "./components/Modal";
export { Badge, badgeVariants, type BadgeProps } from "./components/Badge";
export {
  Avatar,
  AvatarImage,
  AvatarFallback,
  avatarVariants,
  initials,
  type AvatarProps,
} from "./components/Avatar";
export {
  TooltipProvider,
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "./components/Tooltip";
export {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuCheckboxItem,
  DropdownMenuRadioItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuShortcut,
  DropdownMenuGroup,
  DropdownMenuPortal,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuRadioGroup,
} from "./components/DropdownMenu";
export { Tabs, TabsList, TabsTrigger, TabsContent } from "./components/Tabs";
export {
  Sidebar,
  SidebarHeader,
  SidebarBody,
  SidebarFooter,
  SidebarGroup,
  SidebarItem,
  SidebarToggle,
  type SidebarProps,
  type SidebarGroupProps,
  type SidebarItemProps,
} from "./components/Sidebar";
export {
  DataGrid,
  type DataGridColumn,
  type DataGridProps,
  type DataGridSortState,
} from "./components/DataGrid";
export {
  Skeleton,
  skeletonVariants,
  type SkeletonProps,
} from "./components/Skeleton";
export { Spinner, spinnerVariants, type SpinnerProps } from "./components/Spinner";
export { EmptyState, type EmptyStateProps } from "./components/EmptyState";
export {
  Breadcrumb,
  BreadcrumbList,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbPage,
  BreadcrumbSeparator,
  type BreadcrumbLinkProps,
} from "./components/Breadcrumb";
export {
  Stepper,
  type StepperProps,
  type StepperStep,
} from "./components/Stepper";
export {
  StatCard,
  type StatCardProps,
  type StatTrend,
  type StatTrendDirection,
  type StatTrendIntent,
} from "./components/StatCard";
export {
  ConfirmDialog,
  type ConfirmDialogProps,
} from "./components/ConfirmDialog";
export {
  PromptDialog,
  type PromptDialogProps,
} from "./components/PromptDialog";
export {
  Toaster,
  toast,
  useToast,
  toastVariants,
  type ToastItem,
  type ToastOptions,
  type ToastVariant,
  type ToasterProps,
} from "./components/Toast";
export {
  CommandPalette,
  type CommandPaletteProps,
  type CommandItem,
  type CommandGroup,
} from "./components/CommandPalette";
