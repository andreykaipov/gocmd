/*
 * gocmd
 * For the full copyright and license information, please view the LICENSE.txt file.
 */

package flagset

// Arg represents an argument
type Arg struct {
	id        int
	arg       string
	name      string
	value     string
	dash      string
	hasEq     bool
	unnamed   bool
	unset     bool
	kind      string
	flagID    int
	commandID int
	parentID  int
	indexFrom int
	indexTo   int
	updatedBy string
	err       error
}
