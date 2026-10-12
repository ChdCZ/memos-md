// Package memofile keeps every memo as a markdown file on disk.
//
// It is the only package in this fork that adds storage behaviour: the SQL
// store stays the query engine, and this package mirrors it to
// <data>/memos/<uid>.md and reads those files back into the store. The layout
// is deliberately the on-disk shape of the Memos Export Format
// (core/memoexport), so a data directory is also a valid export.
package memofile
